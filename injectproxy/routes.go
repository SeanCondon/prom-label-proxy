// SPDX-FileCopyrightText: 2021-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0
//

// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package injectproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/onosproject/onos-lib-go/pkg/auth"
	"github.com/prometheus-community/prom-label-proxy/pkg/syncv1"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"github.com/efficientgo/tools/core/pkg/merrors"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"log"
	"unicode"
)

const (
	queryParam    = "query"
	matchersParam = "match[]"
)

type routes struct {
	upstream      *url.URL
	handler       http.Handler
	label         string
	adminGroup    string
	defaultGroup  string
	configChannel chan map[string]map[string]string

	mux            *http.ServeMux
	modifiers      map[string]func(*http.Response) error
	errorOnReplace bool
}

type options struct {
	enableLabelAPIs bool
	pasthroughPaths []string
	errorOnReplace  bool
}

type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

// WithEnabledLabelsAPI enables proxying to labels API. If false, "501 Not implemented" will be return for those.
func WithEnabledLabelsAPI() Option {
	return optionFunc(func(o *options) {
		o.enableLabelAPIs = true
	})
}

// WithPassthroughPaths configures routes to register given paths as passthrough handlers for all HTTP methods.
// that, if requested, will be forwarded without enforcing label. Use with care.
// NOTE: Passthrough "all" paths like "/" or "" and regex are not allowed.
func WithPassthroughPaths(paths []string) Option {
	return optionFunc(func(o *options) {
		o.pasthroughPaths = paths
	})
}

// ErrorOnReplace causes the proxy to return 403 if a label matcher we want to
// inject is present in the query already and matches something different
func WithErrorOnReplace() Option {
	return optionFunc(func(o *options) {
		o.errorOnReplace = true
	})
}

// strictMux is a mux that wraps standard HTTP handler with safer handler that allows safe user provided handler registrations.
type strictMux struct {
	seen map[string]struct{}

	m *http.ServeMux
}

func newStrictMux() *strictMux {
	return &strictMux{
		seen: map[string]struct{}{},
		m:    http.NewServeMux(),
	}

}

// Handle is like HTTP mux handle but it does not allow to register paths that are shared with previously registered paths.
// It also makes sure the trailing / is registered too.
// For example if /api/v1/federate was registered consequent registrations like /api/v1/federate/ or /api/v1/federate/some will
// return error. In the mean time request with both /api/v1/federate and /api/v1/federate/ will point to the handled passed by /api/v1/federate
// registration.
// This allows to de-risk ability for user to mis-configure and leak inject isolation.
func (s *strictMux) Handle(pattern string, handler http.Handler) error {
	sanitized := pattern
	for next := strings.TrimSuffix(sanitized, "/"); next != sanitized; sanitized = next {
	}

	if _, ok := s.seen[sanitized]; ok {
		return errors.Errorf("pattern %q was already registered", sanitized)
	}

	for p := range s.seen {
		if strings.HasPrefix(sanitized+"/", p+"/") {
			return errors.Errorf("pattern %q is registered, cannot register path %q that shares it", p, sanitized)
		}
	}

	s.m.Handle(sanitized, handler)
	s.m.Handle(sanitized+"/", handler)
	s.seen[sanitized] = struct{}{}

	return nil
}

func NewRoutes(upstream *url.URL, label string, adminGroup string, defaultGroup string,configChannel chan map[string]map[string]string, opts ...Option) (*routes, error) {
	opt := options{}
	for _, o := range opts {
		o.apply(&opt)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)

	r := &routes{upstream: upstream, handler: proxy, label: label, adminGroup: adminGroup, defaultGroup: defaultGroup, errorOnReplace: opt.errorOnReplace}
	mux := newStrictMux()

	errs := merrors.New(
		mux.Handle("/api/v1/config/", r.updateLabelConfig(enforceMethods(r.matcher, "GET", "POST"))),
		mux.Handle("/federate", r.enforceLabel(enforceMethods(r.matcher, "GET"))),
		mux.Handle("/api/v1/query", r.enforceLabel(enforceMethods(r.query, "GET", "POST"))),
		mux.Handle("/api/v1/query_range", r.enforceLabel(enforceMethods(r.query, "GET", "POST"))),
		mux.Handle("/api/v1/alerts", r.enforceLabel(enforceMethods(r.passthrough, "GET"))),
		mux.Handle("/api/v1/rules", r.enforceLabel(enforceMethods(r.passthrough, "GET"))),
		mux.Handle("/api/v1/series", r.enforceLabel(enforceMethods(r.matcher, "GET" , "POST"))),
		mux.Handle("/api/v1/query_exemplars", r.enforceLabel(enforceMethods(r.query, "GET", "POST"))),
	)
	if opt.enableLabelAPIs {
		errs.Add(
			mux.Handle("/api/v1/labels", r.enforceLabel(enforceMethods(r.matcher, "GET", "POST"))),
			// Full path is /api/v1/label/<label_name>/values but http mux does not support patterns.
			// This is fine though as we don't care about name for matcher injector.
			mux.Handle("/api/v1/label/", r.enforceLabel(enforceMethods(r.matcher, "GET"))),
		)
	}

	errs.Add(
		mux.Handle("/api/v2/silences", r.enforceLabel(enforceMethods(r.silences, "GET", "POST"))),
		mux.Handle("/api/v2/silence/", r.enforceLabel(enforceMethods(r.deleteSilence, "DELETE"))),
	)

	if err := errs.Err(); err != nil {
		return nil, err
	}

	// Validate paths.
	for _, path := range opt.pasthroughPaths {
		u, err := url.Parse(fmt.Sprintf("http://example.com%v", path))
		if err != nil {
			return nil, fmt.Errorf("path %v is not a valid URI path, got %v", path, opt.pasthroughPaths)
		}
		if u.Path != path {
			return nil, fmt.Errorf("path %v is not a valid URI path, got %v", path, opt.pasthroughPaths)
		}
		if u.Path == "" || u.Path == "/" {
			return nil, fmt.Errorf("path %v is not allowed, got %v", u.Path, opt.pasthroughPaths)
		}
	}

	// Register optional passthrough paths.
	for _, path := range opt.pasthroughPaths {
		if err := mux.Handle(path, http.HandlerFunc(r.passthrough)); err != nil {
			return nil, err
		}
	}

	r.mux = mux.m
	r.modifiers = map[string]func(*http.Response) error{
		"/api/v1/rules":  modifyAPIResponse(r.filterRules),
		"/api/v1/alerts": modifyAPIResponse(r.filterAlerts),
	}
	r.configChannel = configChannel
	proxy.ModifyResponse = r.ModifyResponse
	return r, nil
}

func (r *routes) enforceLabel(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {

		if oidc := os.Getenv(auth.OIDCServerURL); oidc != "" {
			groups := enforceAuth(w, req)

			if len(groups) == 0 {
				log.Print("No user group exist for the user ")
				return
			}

			if r.isAdminUser(groups) {
				r.handler.ServeHTTP(w, req)
				return
			}
			//TODO later reintroduce call to get the label config 
			// for now it's not required
			log.Print("Not an admin getting label for the user group")
			result := r.defaultGroup  //setting default group
		        grps := []string { }
			for i := 0 ; i  < len(groups) ; i++ {
			     log.Printf(" groups  %s ", groups[i])
			    // check if it starts with lower case char skip others
			     if unicode.IsLower([]rune(groups[i])[0]) { 
			         grps = append(grps,groups[i])
			     }
			}
		        if len(grps) > 0 {
		           result = strings.Join(grps, "|")
                        }
			log.Printf("setting label config %s = %s ", r.label,result)
			req = req.WithContext(withLabelValue(req.Context(), result))
		} else {

			lvalue := req.FormValue(r.label)
			if lvalue == "" {
				http.Error(w, fmt.Sprintf("Bad request. The %q query parameter must be provided.", r.label), http.StatusBadRequest)
				return
			}
			req = req.WithContext(withLabelValue(req.Context(), lvalue))

		}

		// Remove the proxy label from the query parameters.
		q := req.URL.Query()
		if q.Get(r.label) != "" {
			q.Del(r.label)
		}
		req.URL.RawQuery = q.Encode()
		// Remove the proxy label from the PostForm.
		if req.Method == http.MethodPost {
			if err := req.ParseForm(); err != nil {
				http.Error(w, fmt.Sprintf("Failed to parse the PostForm: %v", err), http.StatusInternalServerError)
				return
			}
			if req.PostForm.Get(r.label) != "" {
				req.PostForm.Del(r.label)
				newBody := req.PostForm.Encode()
				// We are replacing request body, close previous one (req.FormValue ensures it is read fully and not nil).
				_ = req.Body.Close()
				req.Body = ioutil.NopCloser(strings.NewReader(newBody))
				req.ContentLength = int64(len(newBody))
			}
		}
         	h.ServeHTTP(w, req)
	})
}

// API for GET/UPDATE label config for the user groups
func (r *routes) updateLabelConfig(h http.HandlerFunc) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {

		if req.Method == http.MethodPost {
			defer req.Body.Close()
			b, err := io.ReadAll(req.Body)
			if err != nil {
				log.Print(err)
				http.Error(w, fmt.Sprintf("Failed to read the data: %v", err), http.StatusInternalServerError)
				return
			}
			var config syncv1.UserGroups
			err = json.Unmarshal([]byte(string(b)), &config)

			if err != nil {
				log.Print(err)
				http.Error(w, fmt.Sprintf("Failed to parse the data: %v", err), http.StatusInternalServerError)
				return
			}
			values := <-r.configChannel
			for _, usrGrp := range config.UserGroups {
				UserGrpName := usrGrp.Name
				for _, lbl := range usrGrp.Labels {
					if values[UserGrpName] == nil {
						values[UserGrpName] = make(map[string]string)
					}
					values[UserGrpName][lbl.Name] = lbl.Value
				}
			} //
			r.configChannel <- values
		}
		if req.Method == http.MethodGet {
			values := <-r.configChannel
			var UserGrps syncv1.UserGroups
			for UsrName, lbl := range values {
				UserGrp := syncv1.UserGroup{Name: UsrName}
				for key, val := range lbl {
					UserGrp.Labels = append(UserGrp.Labels, syncv1.Label{Name: key, Value: val})
				}
				UserGrps.UserGroups = append(UserGrps.UserGroups, UserGrp)
			}
			strjson, err := json.Marshal(UserGrps)
			r.configChannel <- values
			if err != nil {
				log.Fatal("Failed to unmarshal json ", err)
				http.Error(w, fmt.Sprintf("Failed to unmarshal json %v", err), http.StatusInternalServerError)
				return
			}
			_, err = w.Write([]byte(strjson))
			if err != nil {
				log.Fatal("Failed to write to stream ", err)
				http.Error(w, fmt.Sprintf("Failed to write to stream %v", err), http.StatusInternalServerError)
				return
			}

		}
	})
}

func (r *routes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *routes) ModifyResponse(resp *http.Response) error {
	m, found := r.modifiers[resp.Request.URL.Path]
	if !found {
		// Return the server's response unmodified.
		return nil
	}
	return m(resp)
}

func enforceMethods(h http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		for _, m := range methods {
			if m == req.Method {
				h(w, req)
				return
			}
		}
		http.NotFound(w, req)
	}
}

type ctxKey int

const keyLabel ctxKey = iota

func mustLabelValue(ctx context.Context) string {
	label, ok := ctx.Value(keyLabel).(string)
	if !ok {
		panic(fmt.Sprintf("can't find the %q value in the context", keyLabel))
	}
	if label == "" {
		panic(fmt.Sprintf("empty %q value in the context", keyLabel))
	}
	return label
}

func withLabelValue(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, keyLabel, label)
}

func (r *routes) passthrough(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *routes) query(w http.ResponseWriter, req *http.Request) {
	e := NewEnforcer(r.errorOnReplace,
		[]*labels.Matcher{{
			Name:  r.label,
			Type:  labels.MatchEqual,
			Value: mustLabelValue(req.Context()),
		}}...)

	// The `query` can come in the URL query string and/or the POST body.
	// For this reason, we need to try to enforcing in both places.
	// Note: a POST request may include some values in the URL query string
	// and others in the body. If both locations include a `query`, then
	// enforce in both places.
	q, found1, err := enforceQueryValues(e, req.URL.Query())
	if err != nil {
		if _, ok := err.(IllegalLabelMatcherError); ok {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	req.URL.RawQuery = q

	var found2 bool
	// Enforce the query in the POST body if needed.
	if req.Method == http.MethodPost {
		if err := req.ParseForm(); err != nil {
			return
		}
		q, found2, err = enforceQueryValues(e, req.PostForm)
		if err != nil {
			if _, ok := err.(IllegalLabelMatcherError); ok {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		// We are replacing request body, close previous one (ParseForm ensures it is read fully and not nil).
		_ = req.Body.Close()
		req.Body = ioutil.NopCloser(strings.NewReader(q))
		req.ContentLength = int64(len(q))
	}

	// If no query was found, return early.
	if !found1 && !found2 {
		return
	}

	r.handler.ServeHTTP(w, req)
}

func enforceQueryValues(e *Enforcer, v url.Values) (values string, noQuery bool, err error) {
	// If no values were given or no query is present,
	// e.g. because the query came in the POST body
	// but the URL query string was passed, then finish early.
	if v.Get(queryParam) == "" {
		return v.Encode(), false, nil
	}
	expr, err := parser.ParseExpr(v.Get(queryParam))
	if err != nil {
		return "", true, err
	}

	if err := e.EnforceNode(expr); err != nil {
		return "", true, err
	}

	v.Set(queryParam, expr.String())
	return v.Encode(), true, nil
}

// matcher ensures all the provided match[] if any has label injected. If none was provided, single matcher is injected.
// This works for non-query Prometheus APIs like: /api/v1/series, /api/v1/label/<name>/values, /api/v1/labels and /federate support multiple matchers.
// See e.g https://prometheus.io/docs/prometheus/latest/querying/api/#querying-metadata
func (r *routes) matcher(w http.ResponseWriter, req *http.Request) {
	matcher := &labels.Matcher{
		Name:  r.label,
		Type:  labels.MatchEqual,
		Value: mustLabelValue(req.Context()),
	}
	q := req.URL.Query()
	if err := injectMatcher(q, matcher); err != nil {
		return
	}
	req.URL.RawQuery = q.Encode()
	if req.Method == http.MethodPost {
		if err := req.ParseForm(); err != nil {
			return
		}
		q = req.PostForm
		if err := injectMatcher(q, matcher); err != nil {
			return
		}
		// We are replacing request body, close previous one (ParseForm ensures it is read fully and not nil).
		_ = req.Body.Close()
		newBody := q.Encode()
		req.Body = ioutil.NopCloser(strings.NewReader(newBody))
		req.ContentLength = int64(len(newBody))
	}
	r.handler.ServeHTTP(w, req)
}

func injectMatcher(q url.Values, matcher *labels.Matcher) error {
	matchers := q[matchersParam]
	if len(matchers) == 0 {
		q.Set(matchersParam, matchersToString(matcher))
	} else {
		// Inject label to existing matchers.
		for i, m := range matchers {
			ms, err := parser.ParseMetricSelector(m)
			if err != nil {
				return err
			}
			matchers[i] = matchersToString(append(ms, matcher)...)
		}
		q[matchersParam] = matchers
	}
	return nil
}

func matchersToString(ms ...*labels.Matcher) string {
	var el []string
	for _, m := range ms {
		el = append(el, m.String())
	}
	return fmt.Sprintf("{%v}", strings.Join(el, ","))
}
