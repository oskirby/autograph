// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor: Julien Vehent jvehent@mozilla.com [:ulfr]

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/mozilla-services/autograph/database"
	"github.com/mozilla-services/autograph/formats"
	"github.com/mozilla-services/autograph/signer"
	"github.com/mozilla-services/autograph/signer/apk2"
	"github.com/mozilla-services/autograph/signer/contentsignature"
	"github.com/mozilla-services/autograph/signer/xpi"

	"go.mozilla.org/hawk"

	margo "go.mozilla.org/mar"
)

type HandlerTestCase struct {
	name   string
	method string
	url    string

	// urlRouteVars are https://pkg.go.dev/github.com/gorilla/mux#Vars
	// as configured with the handler at /config/{keyid:[a-zA-Z0-9-_]{1,64}}
	// there should only be a keyid var and it should match the url value
	urlRouteVars map[string]string

	// headers are additional http headers to set
	headers *http.Header

	// user/auth ID to build an Authorization header for
	authorizeID string
	nilBody     bool
	body        string

	expectedStatus  int
	expectedHeaders http.Header
	expectedBody    string
}

func (testcase *HandlerTestCase) NewRequest(t *testing.T) *http.Request {
	// test request setup
	var (
		req *http.Request
		err error
	)
	if testcase.nilBody {
		req, err = http.NewRequest(testcase.method, testcase.url, nil)
	} else {
		req, err = http.NewRequest(testcase.method, testcase.url, strings.NewReader(testcase.body))
	}
	if err != nil {
		t.Fatal(err)
	}
	req = mux.SetURLVars(req, testcase.urlRouteVars)

	if testcase.headers != nil {
		req.Header = *testcase.headers
	}
	if testcase.authorizeID != "" {
		auth, err := ag.getAuthByID(testcase.authorizeID)
		if err != nil {
			t.Fatal(err)
		}
		// getAuthHeader requires a content type and body
		req.Header.Set("Authorization", hawk.NewRequestAuth(req,
			&hawk.Credentials{
				ID:   auth.ID,
				Key:  auth.Key,
				Hash: sha256.New},
			0).RequestHeader())
	}

	return req
}

func (testcase *HandlerTestCase) ValidateResponse(t *testing.T, w *httptest.ResponseRecorder) {
	if w.Code != testcase.expectedStatus {
		t.Fatalf("test case %s: got code %d but expected %d",
			testcase.name, w.Code, testcase.expectedStatus)
	}
	if w.Body.String() != testcase.expectedBody {
		t.Fatalf("test case %s: got body %q expected %q", testcase.name, w.Body.String(), testcase.expectedBody)
	}
	for expectedHeader, expectedHeaderVals := range testcase.expectedHeaders {
		vals, ok := w.Header()[expectedHeader]
		if !ok {
			t.Fatalf("test case %s: expected header %q not found", testcase.name, expectedHeader)
		}
		if strings.Join(vals, "") != strings.Join(expectedHeaderVals, "") {
			t.Fatalf("test case %s: header vals %q did not match expected %q ", testcase.name, vals, expectedHeaderVals)
		}
	}
}

func (testcase *HandlerTestCase) Run(t *testing.T, handler func(http.ResponseWriter, *http.Request)) {
	// test request setup
	var req = testcase.NewRequest(t)

	// run the request
	w := httptest.NewRecorder()
	handler(w, req)

	// validate response
	testcase.ValidateResponse(t, w)
}

func TestBadRequest(t *testing.T) {
	t.Parallel()

	var TESTCASES = []struct {
		endpoint string
		method   string
		body     string
	}{
		// missing request body
		{`/sign/data`, `POST`, ``},
		{`/sign/hash`, `POST`, ``},
		{`/sign/file`, `POST`, ``},
		{`/sign/files`, `POST`, ``},
		// invalid json body
		{`/sign/data`, `POST`, `{|||...........`},
		{`/sign/hash`, `POST`, `{|||...........`},
		{`/sign/file`, `POST`, `{|||...........`},
		{`/sign/files`, `POST`, `{|||...........`},
		// missing input
		{`/sign/data`, `POST`, `[{"input": ""}]`},
		{`/sign/hash`, `POST`, `[{"input": ""}]`},
		{`/sign/file`, `POST`, `[{"input": ""}]`},
		{`/sign/files`, `POST`, `[{"input": ""}]`},
		// input not in base64
		{`/sign/data`, `POST`, `[{"input": "......."}]`},
		{`/sign/hash`, `POST`, `[{"input": "......."}]`},
		{`/sign/file`, `POST`, `[{"input": "......."}]`},
		{`/sign/files`, `POST`, `[{"input": "......."}]`},

		// missing files
		{`/sign/files`, `POST`, `[{"input": "aGVsbG8=", "keyid": "randompgp-debsign"}]`},
		// files is an empty string
		{`/sign/files`, `POST`, `[{"files": "", "keyid": "randompgp-debsign"}]`},
		// files is a base64 string
		{`/sign/files`, `POST`, `[{"files": "aGVsbG8=", "keyid": "randompgp-debsign"}]`},
		// files is an empty array
		{`/sign/files`, `POST`, `[{"files": [], "keyid": "randompgp-debsign"}]`},
		// files content is not valid base64
		{`/sign/files`, `POST`, `[{"files": [{"name": "0", "content":"...."}], "keyid": "randompgp-debsign"}]`},
		// file name includes relative current directory: ./foo.dsc
		{`/sign/files`, `POST`, `[{"files": [{"name": "./foo.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name includes relative parent directory: ../../foo.dsc
		{`/sign/files`, `POST`, `[{"files": [{"name": "../../foo.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name includes relative parent directory following a filename: cwd/../../foo.dsc
		{`/sign/files`, `POST`, `[{"files": [{"name": "cwd/../../foo.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name includes two dots with otherwise valid chars: cwd..foo.dsc
		{`/sign/files`, `POST`, `[{"files": [{"name": "cwd..foo.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name starts with dot: .bashrc.dsc
		{`/sign/files`, `POST`, `[{"files": [{"name": ".bashrc.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name starts with /: /etc
		{`/sign/files`, `POST`, `[{"files": [{"name": "/etc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name is path spam/eggs/foo.dsc
		{`/sign/files`, `POST`, `[{"files": [{"name": "spam/eggs/foo.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name is windows path
		{`/sign/files`, `POST`, `[{"files": [{"name": "C:\spam\eggs\foo.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name is file:// url
		{`/sign/files`, `POST`, `[{"files": [{"name": "file:///etc/hosts", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name with @
		{`/sign/files`, `POST`, `[{"files": [{"name": "file@localhost.wtf", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name beginning with \0
		{`/sign/files`, `POST`, `[{"files": [{"name": "\0file", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name containing \0
		{`/sign/files`, `POST`, `[{"files": [{"name": "\0/file", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},
		// file name is >255 chars (404 long)
		{`/sign/files`, `POST`, `[{"files": [{"name": "spamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspamspam.dsc", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},

		// files has too many files (33)
		{`/sign/files`, `POST`, `[{"files": [{"name": "0", "content":"aGVsbG8="}, {"name": "1", "content":"aGVsbG8="}, {"name": "2", "content":"aGVsbG8="}, {"name": "3", "content":"aGVsbG8="}, {"name": "4", "content":"aGVsbG8="}, {"name": "5", "content":"aGVsbG8="}, {"name": "6", "content":"aGVsbG8="}, {"name": "7", "content":"aGVsbG8="}, {"name": "8", "content":"aGVsbG8="}, {"name": "9", "content":"aGVsbG8="}, {"name": "10", "content":"aGVsbG8="}, {"name": "11", "content":"aGVsbG8="}, {"name": "12", "content":"aGVsbG8="}, {"name": "13", "content":"aGVsbG8="}, {"name": "14", "content":"aGVsbG8="}, {"name": "15", "content":"aGVsbG8="}, {"name": "16", "content":"aGVsbG8="}, {"name": "17", "content":"aGVsbG8="}, {"name": "18", "content":"aGVsbG8="}, {"name": "19", "content":"aGVsbG8="}, {"name": "20", "content":"aGVsbG8="}, {"name": "21", "content":"aGVsbG8="}, {"name": "22", "content":"aGVsbG8="}, {"name": "23", "content":"aGVsbG8="}, {"name": "24", "content":"aGVsbG8="}, {"name": "25", "content":"aGVsbG8="}, {"name": "26", "content":"aGVsbG8="}, {"name": "27", "content":"aGVsbG8="}, {"name": "28", "content":"aGVsbG8="}, {"name": "29", "content":"aGVsbG8="}, {"name": "30", "content":"aGVsbG8="}, {"name": "31", "content":"aGVsbG8="}, {"name": "32", "content":"aGVsbG8="}, {"name": "33", "content":"aGVsbG8="}], "keyid": "randompgp-debsign"}]`},

		// asking for a xpi signature using /sign/hash fails
		{`/sign/hash`, `POST`, `[{"input": "Y2FyaWJvdW1hdXJpY2UK", "keyid": "webextensions-rsa"}]`},
	}
	for i, testcase := range TESTCASES {
		i := i
		testcase := testcase

		t.Run(fmt.Sprintf("returns 400 for invalid %s %s %s", testcase.method, testcase.endpoint, testcase.body), func(t *testing.T) {
			t.Parallel()

			body := strings.NewReader(testcase.body)
			req, err := http.NewRequest(testcase.method, "http://foo.bar"+testcase.endpoint, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			auth, err := ag.getAuthByID(conf.Authorizations[0].ID)
			if err != nil {
				t.Fatal(err)
			}

			authheader := getAuthHeader(req,
				auth.ID,
				auth.Key,
				sha256.New, id(),
				"application/json",
				[]byte(testcase.body))
			req.Header.Set("Authorization", authheader)
			w := httptest.NewRecorder()
			ag.handleSignature(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("test case %d %s %s %q should have failed, but succeeded with %d: %s", i, testcase.method, testcase.endpoint, testcase.body, w.Code, w.Body.String())
			}
			// t.Logf("failed with %d: %s", w.Code, w.Body.String())
		})
	}
}

func TestRequestTooLarge(t *testing.T) {
	t.Parallel()

	blob := strings.Repeat("foobar", 200)
	body := strings.NewReader(blob)
	req, err := http.NewRequest("GET", "http://foo.bar/sign/data", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := ag.getAuthByID(conf.Authorizations[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	authheader := getAuthHeader(req,
		auth.ID,
		auth.Key,
		sha256.New, id(),
		"application/json",
		[]byte(blob))
	req.Header.Set("Authorization", authheader)
	w := httptest.NewRecorder()
	ag.handleSignature(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("large request should have failed, but succeeded with %d: %s", w.Code, w.Body.String())
	}
}

func TestBadContentType(t *testing.T) {
	t.Parallel()

	blob := "foofoofoofoofoofoofoofoofoofoofoofoofoofoo"
	body := strings.NewReader(blob)
	req, err := http.NewRequest("GET", "http://foo.bar/sign/data", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/foobar")
	auth, err := ag.getAuthByID(conf.Authorizations[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	authheader := getAuthHeader(req,
		auth.ID,
		auth.Key,
		sha256.New, id(),
		"application/foobar",
		[]byte(blob))
	req.Header.Set("Authorization", authheader)
	w := httptest.NewRecorder()
	ag.handleSignature(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad content type request should have failed, but succeeded with %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthFail(t *testing.T) {
	t.Parallel()

	var TESTCASES = []struct {
		user        string
		token       string
		hash        func() hash.Hash
		contenttype string
		body        string
	}{
		// test bad user
		{`baduser`, `fs5wgcer9qj819kfptdlp8gm227ewxnzvsuj9ztycsx08hfhzu`, sha256.New, `application/json`, `[{"input":"y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d"}]`},
		// test bad token
		{`tester`, `badtoken`, sha256.New, `application/json`, `[{"input":"y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d"}]`},
		// test wrong hash
		{`tester`, `fs5wgcer9qj819kfptdlp8gm227ewxnzvsuj9ztycsx08hfhzu`, sha512.New, `application/json`, `[{"input":"y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d"}]`},
		// test wrong content type
		{`tester`, `fs5wgcer9qj819kfptdlp8gm227ewxnzvsuj9ztycsx08hfhzu`, sha256.New, `test/plain`, `[{"input":"y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d"}]`},
		// test missing payload
		{`tester`, `fs5wgcer9qj819kfptdlp8gm227ewxnzvsuj9ztycsx08hfhzu`, sha256.New, `application/json`, ``},
	}
	for i, testcase := range TESTCASES {
		body := strings.NewReader(`[{"input":"y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d"}]`)
		req, err := http.NewRequest("POST", "http://foo.bar/sign/data", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		authheader := getAuthHeader(req, testcase.user, testcase.token, testcase.hash, id(), testcase.contenttype, []byte(testcase.body))
		req.Header.Set("Authorization", authheader)
		t.Log(i, authheader)
		w := httptest.NewRecorder()
		ag.handleSignature(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("test case %d was authorized with %d and should have failed; authorization header was: %s; response was: %s",
				i, w.Code, req.Header.Get("Authorization"), w.Body.String())
		}
	}
}

func TestLBHeartbeat(t *testing.T) {
	t.Parallel()

	var TESTCASES = []struct {
		expect int
		method string
	}{
		{http.StatusOK, `GET`},
		{http.StatusMethodNotAllowed, `POST`},
		{http.StatusMethodNotAllowed, `PUT`},
		{http.StatusMethodNotAllowed, `HEAD`},
	}
	for i, testcase := range TESTCASES {
		req, err := http.NewRequest(testcase.method, "http://foo.bar/__lbheartbeat__", nil)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		handleLBHeartbeat(w, req)
		if w.Code != testcase.expect {
			t.Fatalf("test case %d failed with code %d but %d was expected",
				i, w.Code, testcase.expect)
		}
	}
}

func TestHeartbeat(t *testing.T) {
	t.Parallel()

	var TESTCASES = []HandlerTestCase{
		{
			name:           "returns 200 for GET",
			method:         "GET",
			url:            "http://foo.bar/__heartbeat__",
			expectedStatus: http.StatusOK,
			expectedBody:   "{}",
		},
		{
			name:           "returns 405 for POST",
			method:         "POST",
			url:            "http://foo.bar/__heartbeat__",
			expectedStatus: http.StatusMethodNotAllowed,
			expectedBody:   "POST method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
		},
		{
			name:           "returns 405 for PUT",
			method:         "PUT",
			url:            "http://foo.bar/__heartbeat__",
			expectedStatus: http.StatusMethodNotAllowed,
			expectedBody:   "PUT method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
		},
		{
			name:           "returns 405 for HEAD",
			method:         "HEAD",
			url:            "http://foo.bar/__heartbeat__",
			expectedStatus: http.StatusMethodNotAllowed,
			expectedBody:   "HEAD method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
		},
	}
	for _, testcase := range TESTCASES {
		testcase.Run(t, ag.handleHeartbeat)
	}
}

func TestHeartbeatChecksHSMStatusFails(t *testing.T) {
	// NB: do not run in parallel with TestHeartbeat*
	ag.heartbeatConf = &heartbeatConfig{
		HSMCheckTimeout: time.Second,
		hsmSignerConf:   &ag.getSigners()[0].(*contentsignature.ContentSigner).Configuration,
	}

	var testcase = HandlerTestCase{
		name:           "returns 500 for GET with HSM inaccessible",
		method:         "GET",
		url:            "http://foo.bar/__heartbeat__",
		expectedStatus: http.StatusInternalServerError,
		expectedBody:   "{\"hsmAccessible\":false}",
	}
	testcase.Run(t, ag.handleHeartbeat)

	ag.heartbeatConf = nil
}

func TestHeartbeatChecksHSMStatusFailsWhenNotConfigured(t *testing.T) {
	// NB: do not run in parallel with TestHeartbeat*
	ag.heartbeatConf = nil

	var testcase = HandlerTestCase{
		name:           "returns 500 for GET without heartbeat config HSM",
		method:         "GET",
		url:            "http://foo.bar/__heartbeat__",
		expectedStatus: http.StatusInternalServerError,
		expectedBody:   "Missing heartbeat config\r\nrequest-id: -\n",
	}
	testcase.Run(t, ag.handleHeartbeat)
}

func TestHeartbeatChecksDBStatusOKAndTimesout(t *testing.T) {
	// NB: do not run in parallel with TestHeartbeat* or DB tests
	host := database.GetTestDBHost()
	db, err := database.Connect(database.Config{
		Name:     "autograph",
		User:     "myautographdbuser",
		Password: "myautographdbpassword",
		Host:     host + ":5432",
	})
	if err != nil {
		t.Fatal(err)
	}
	ag.db = db
	ag.heartbeatConf = &heartbeatConfig{
		DBCheckTimeout: 2 * time.Second,
	}

	var dbAccessibleTestCase = HandlerTestCase{
		name:           "returns 200 for GET with DB accessible",
		method:         "GET",
		url:            "http://foo.bar/__heartbeat__",
		expectedStatus: http.StatusOK,
		expectedBody:   "{\"dbAccessible\":true}",
	}
	dbAccessibleTestCase.Run(t, ag.handleHeartbeat)

	// drop timeout
	ag.heartbeatConf.DBCheckTimeout = 1 * time.Nanosecond
	// check DB request times out
	var dbTimeoutTestCase = HandlerTestCase{
		name:           "returns 200 for GET with DB time out",
		method:         "GET",
		url:            "http://foo.bar/__heartbeat__",
		expectedStatus: http.StatusOK,
		expectedBody:   "{\"dbAccessible\":false}",
	}
	dbTimeoutTestCase.Run(t, ag.handleHeartbeat)

	// restore longer timeout and close the DB connection
	ag.heartbeatConf.DBCheckTimeout = 1 * time.Second
	db.Close()
	// check DB request still fails
	var dbOfflineTestCase = HandlerTestCase{
		name:           "returns 200 for GET with DB inaccessible",
		method:         "GET",
		url:            "http://foo.bar/__heartbeat__",
		expectedStatus: http.StatusOK,
		expectedBody:   "{\"dbAccessible\":false}",
	}
	dbOfflineTestCase.Run(t, ag.handleHeartbeat)

	ag.db = nil
}

func TestVersion(t *testing.T) {
	t.Parallel()

	var TESTCASES = []struct {
		expect int
		method string
	}{
		{http.StatusOK, `GET`},
		{http.StatusMethodNotAllowed, `POST`},
		{http.StatusMethodNotAllowed, `PUT`},
		{http.StatusMethodNotAllowed, `HEAD`},
	}
	for i, testcase := range TESTCASES {
		req, err := http.NewRequest(testcase.method, "http://foo.bar/__version__", nil)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		handleVersion(w, req)
		if w.Code != testcase.expect {
			t.Fatalf("test case %d failed with code %d but %d was expected",
				i, w.Code, testcase.expect)
		}
	}
}

// verify that user `alice` and `bob` are allowed to sign
// with their respective keys:
// * `appkey1` and `appkey2` for `alice`
// * `appkey2` only for `bob`
func TestSignerAuthorized(t *testing.T) {
	t.Parallel()

	var TESTCASES = []struct {
		userid string
		sgs    []formats.SignatureRequest
	}{
		{
			userid: conf.Authorizations[0].ID,
			sgs: []formats.SignatureRequest{
				formats.SignatureRequest{
					Input: "PCFET0NUWVBFIEhUTUw+CjxodG1sPgo8IS0tIGh0dHBzOi8vYnVnemlsbGEubW96aWxsYS5vcmcvc2hvd19idWcuY2dpP2lkPTEyMjY5MjggLS0+CjxoZWFkPgogIDxtZXRhIGNoYXJzZXQ9InV0Zi04Ij4KICA8dGl0bGU+VGVzdHBhZ2UgZm9yIGJ1ZyAxMjI2OTI4PC90aXRsZT4KPC9oZWFkPgo8Ym9keT4KICBKdXN0IGEgZnVsbHkgZ29vZCB0ZXN0cGFnZSBmb3IgQnVnIDEyMjY5Mjg8YnIvPgo8L2JvZHk+CjwvaHRtbD4K",
					KeyID: conf.Authorizations[0].Signers[0],
				},
				formats.SignatureRequest{
					Input: "y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d",
					KeyID: conf.Authorizations[0].Signers[0],
				},
				formats.SignatureRequest{
					Input: "Q29udGVudC1TaWduYXR1cmU6ADwhRE9DVFlQRSBIVE1MPgo8aHRtbD4KPCEtLSBodHRwczovL2J1Z3ppbGxhLm1vemlsbGEub3JnL3Nob3dfYnVnLmNnaT9pZD0xMjI2OTI4IC0tPgo8aGVhZD4KICA8bWV0YSBjaGFyc2V0PSJ1dGYtOCI+CiAgPHRpdGxlPlRlc3RwYWdlIGZvciBidWcgMTIyNjkyODwvdGl0bGU+CjwvaGVhZD4KPGJvZHk+CiAgSnVzdCBhIGZ1bGx5IGdvb2QgdGVzdHBhZ2UgZm9yIEJ1ZyAxMjI2OTI4PGJyLz4KPC9ib2R5Pgo8L2h0bWw+Cg==",
					KeyID: conf.Authorizations[0].Signers[1],
				},
			},
		},
		{
			userid: conf.Authorizations[1].ID,
			sgs: []formats.SignatureRequest{
				formats.SignatureRequest{
					Input: "PCFET0NUWVBFIEhUTUw+CjxodG1sPgo8IS0tIGh0dHBzOi8vYnVnemlsbGEubW96aWxsYS5vcmcvc2hvd19idWcuY2dpP2lkPTEyMjY5MjggLS0+CjxoZWFkPgogIDxtZXRhIGNoYXJzZXQ9InV0Zi04Ij4KICA8dGl0bGU+VGVzdHBhZ2UgZm9yIGJ1ZyAxMjI2OTI4PC90aXRsZT4KPC9oZWFkPgo8Ym9keT4KICBKdXN0IGEgZnVsbHkgZ29vZCB0ZXN0cGFnZSBmb3IgQnVnIDEyMjY5Mjg8YnIvPgo8L2JvZHk+CjwvaHRtbD4K",
					KeyID: conf.Authorizations[1].Signers[0],
				},
				formats.SignatureRequest{
					Input: "y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d",
					KeyID: conf.Authorizations[1].Signers[0],
				},
			},
		},
	}
	for tid, testcase := range TESTCASES {
		userid := testcase.userid
		body, err := json.Marshal(testcase.sgs)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s", body)
		rdr := bytes.NewReader(body)
		req, err := http.NewRequest("POST", "http://foo.bar/sign/data", rdr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		auth, err := ag.getAuthByID(userid)
		if err != nil {
			t.Fatal(err)
		}

		authheader := getAuthHeader(req, auth.ID, auth.Key,
			sha256.New, id(), "application/json", body)
		req.Header.Set("Authorization", authheader)
		w := httptest.NewRecorder()
		ag.handleSignature(w, req)
		if w.Code != http.StatusCreated || w.Body.String() == "" {
			t.Fatalf("test case %d failed with %d: %s; request was: %+v",
				tid, w.Code, w.Body.String(), req)
		}
		// verify that we got a proper signature response, with a valid signature
		var responses []formats.SignatureResponse
		err = json.Unmarshal(w.Body.Bytes(), &responses)
		if err != nil {
			t.Fatal(err)
		}
		if len(responses) != len(testcase.sgs) {
			t.Fatalf("test case %d failed to receive as many responses (%d) as we sent requests (%d)",
				tid, len(responses), len(testcase.sgs))
		}
		for i, response := range responses {
			input, err := base64.StdEncoding.DecodeString(testcase.sgs[i].Input)
			if err != nil {
				t.Fatalf("test case %d input data decode error: %v", tid, err)
			}
			err = contentsignature.VerifyResponse(input, response)
			if err != nil {
				t.Fatalf("test case %d signature verification failed in response %d; request was: %+v",
					tid, i, req)
			}
		}
	}
}

// verify that user `bob` is not allowed to sign with `appkey1`
func TestSignerUnauthorized(t *testing.T) {
	t.Parallel()

	var TESTCASES = []formats.SignatureRequest{
		// request signature that need to prepend the content-signature:\x00 header
		formats.SignatureRequest{
			Input: "PCFET0NUWVBFIEhUTUw+CjxodG1sPgo8IS0tIGh0dHBzOi8vYnVnemlsbGEubW96aWxsYS5vcmcvc2hvd19idWcuY2dpP2lkPTEyMjY5MjggLS0+CjxoZWFkPgogIDxtZXRhIGNoYXJzZXQ9InV0Zi04Ij4KICA8dGl0bGU+VGVzdHBhZ2UgZm9yIGJ1ZyAxMjI2OTI4PC90aXRsZT4KPC9oZWFkPgo8Ym9keT4KICBKdXN0IGEgZnVsbHkgZ29vZCB0ZXN0cGFnZSBmb3IgQnVnIDEyMjY5Mjg8YnIvPgo8L2JvZHk+CjwvaHRtbD4K",
			KeyID: conf.Authorizations[0].Signers[0],
		},
		formats.SignatureRequest{
			Input: "y0hdfsN8tHlCG82JLywb4d2U+VGWWry8dzwIC3Hk6j32mryUHxUel9SWM5TWkk0d",
			KeyID: conf.Authorizations[0].Signers[0],
		},
	}
	userid := conf.Authorizations[1].ID
	body, err := json.Marshal(TESTCASES)
	if err != nil {
		t.Fatal(err)
	}
	rdr := bytes.NewReader(body)
	req, err := http.NewRequest("POST", "http://foo.bar/sign/data", rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := ag.getAuthByID(userid)
	if err != nil {
		t.Fatal(err)
	}
	authheader := getAuthHeader(req, auth.ID, auth.Key,
		sha256.New, id(), "application/json", body)
	req.Header.Set("Authorization", authheader)
	w := httptest.NewRecorder()
	ag.handleSignature(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected to fail with %d but got %d: %s; request was: %+v", http.StatusUnauthorized, w.Code, w.Body.String(), req)
	}
}

func TestContentType(t *testing.T) {
	t.Parallel()

	var TESTCASES = []formats.SignatureRequest{
		formats.SignatureRequest{
			Input: "Y2FyaWJvdXZpbmRpZXV4Cg==",
		},
	}
	userid := conf.Authorizations[0].ID
	body, err := json.Marshal(TESTCASES)
	if err != nil {
		t.Fatal(err)
	}
	rdr := bytes.NewReader(body)
	req, err := http.NewRequest("POST", "http://foo.bar/sign/data", rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := ag.getAuthByID(userid)
	if err != nil {
		t.Fatal(err)
	}
	authheader := getAuthHeader(req, auth.ID, auth.Key,
		sha256.New, id(), "application/json", body)
	req.Header.Set("Authorization", authheader)
	w := httptest.NewRecorder()
	ag.handleSignature(w, req)
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("expected response with content type 'application/json' but got %q instead",
			w.Header().Get("Content-Type"))
	}
}

func TestDebug(t *testing.T) {
	ag.enableDebug()
	if !ag.debug {
		t.Fatalf("expected debug mode to be enabled, but is disabled")
	}
	ag.disableDebug()
	if ag.debug {
		t.Fatalf("expected debug mode to be disabled, but is enabled")
	}
}

func TestHandleGetAuthKeyIDs(t *testing.T) {
	t.Parallel()

	const autographDevAliceKeyIDsJSON = "[\"apk_cert_with_ecdsa_sha256\",\"apk_cert_with_ecdsa_sha256_v3\",\"appkey1\",\"appkey2\",\"dummyrsa\",\"dummyrsapss\",\"extensions-ecdsa\",\"extensions-ecdsa-expired-chain\",\"legacy_apk_with_rsa\",\"normandy\",\"pgpsubkey\",\"pgpsubkey-debsign\",\"randompgp\",\"randompgp-debsign\",\"remote-settings\",\"testapp-android\",\"testapp-android-legacy\",\"testapp-android-v3\",\"testauthenticode\",\"testmar\",\"testmarecdsa\",\"webextensions-rsa\",\"webextensions-rsa-with-recommendation\"]"

	var testcases = []HandlerTestCase{
		{
			name:            "invalid method POST returns 405",
			method:          "POST",
			url:             "http://foo.bar/auths/alice/keyids",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "POST method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "invalid method PUT returns 405",
			method:          "PUT",
			url:             "http://foo.bar/auths/alice/keyids",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "PUT method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "invalid method OPTIONS returns 405",
			method:          "OPTIONS",
			url:             "http://foo.bar/auths/alice/keyids",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "OPTIONS method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "invalid method HEAD returns 405",
			method:          "HEAD",
			url:             "http://foo.bar/auths/alice/keyids",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "HEAD method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with empty body returns 200",
			method:          "GET",
			url:             "http://foo.bar/auths/alice/keyids",
			urlRouteVars:    map[string]string{"auth_id": "alice"},
			nilBody:         false,
			body:            "",
			authorizeID:     "alice",
			expectedStatus:  http.StatusOK,
			expectedBody:    autographDevAliceKeyIDsJSON,
			expectedHeaders: http.Header{"Content-Type": []string{"application/json"}},
		},
		{
			name:            "GET with non-empty body returns 400",
			method:          "GET",
			url:             "http://foo.bar/auths/alice/keyids",
			urlRouteVars:    map[string]string{"auth_id": "alice"},
			nilBody:         false,
			body:            "foobar/---",
			expectedStatus:  http.StatusBadRequest,
			expectedBody:    "endpoint received unexpected request body\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with misconfigured auth_id route param returns 500",
			method:          "GET",
			url:             "http://foo.bar/auths/alice/keyids",
			urlRouteVars:    map[string]string{}, // missing auth_id
			nilBody:         true,
			expectedStatus:  http.StatusInternalServerError,
			expectedBody:    "route is improperly configured\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET missing Authorization header returns 401",
			method:          "GET",
			url:             "http://foo.bar/auths/alice/keyids",
			urlRouteVars:    map[string]string{"auth_id": "alice"},
			nilBody:         true,
			expectedStatus:  http.StatusUnauthorized,
			expectedBody:    "authorization verification failed: missing Authorization header\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with invalid Authorization header returns 401",
			method:          "GET",
			url:             "http://foo.bar/auths/alice/keyids",
			urlRouteVars:    map[string]string{"auth_id": "alice"},
			headers:         &http.Header{"Authorization": []string{`Hawk id="dh37fgj492je", ts="1353832234", nonce="j4h3g2", ext="some-app-ext-data", mac="6R4rV5iE+NPoym+WwjeHzjAGXUtLNIxmo1vpMofpLAE="`}},
			nilBody:         true,
			expectedStatus:  http.StatusUnauthorized,
			expectedBody:    "authorization verification failed: hawk: credential error with id dh37fgj492je and app : unknown id\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with invalid auth id url param returns 400",
			method:          "GET",
			url:             "http://foo.bar/auths//keyids",
			urlRouteVars:    map[string]string{"auth_id": ""},
			nilBody:         true,
			expectedStatus:  http.StatusBadRequest,
			expectedBody:    "auth_id in URL path '' is invalid, it must match ^[a-zA-Z0-9-_]{1,255}$\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with auth returns 403 for mismatched auth ids (alice cannot get bob's keyids)",
			method:          "GET",
			url:             "http://foo.bar/auths/bob/keyids",
			urlRouteVars:    map[string]string{"auth_id": "bob"},
			nilBody:         true,
			authorizeID:     "alice",
			expectedStatus:  http.StatusForbidden,
			expectedBody:    "Authorized user \"alice\" cannot request keyids for user \"bob\"\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with auth returns 200",
			method:          "GET",
			url:             "http://foo.bar/auths/alice/keyids",
			urlRouteVars:    map[string]string{"auth_id": "alice"},
			nilBody:         true,
			authorizeID:     "alice",
			expectedStatus:  http.StatusOK,
			expectedBody:    autographDevAliceKeyIDsJSON,
			expectedHeaders: http.Header{"Content-Type": []string{"application/json"}},
		},
	}
	for _, testcase := range testcases {
		testcase.Run(t, ag.handleGetAuthKeyIDs)
	}
}

func TestHandleGetConfig(t *testing.T) {
	t.Parallel()

	// A signer that alice can access
	var autographDummyRsaConfig = signer.SanitizedConfig{
		ID:         "dummyrsa",
		Type:       "genericrsa",
		Mode:       "pss",
		PublicKey:  "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAtEM/Vdfd4Vl9wmeVdCYuWYnQl0Zc9RW5hLE4hFA+c277qanE8XCK+ap/c5so87XngLLfacB3zZhGxIOut/4SlEBOAUmVNCfnTO+YkRk3A8OyJ4XNqdn+/ov78ZbssGf+0zws2BcwZYwhtuTvro3yi62FQ7T1TpT5VjljH7sHW/iZnS/RKiY4DwqAN799gkB+Gwovtroabh2w5OX0P+PYyUbJLFQeo5uiAQ8cAXTlHqCkj11GYgU4ttVDuFGotKRyaRn1F+yKxE4LQcAULx7s0KzvS35mNU+MoywLWjy9a4TcjK0nq+BjspKX4UkNwVstvH18hQWun7E+dxTi59cRmwIDAQAB",
		Hash:       "sha256",
		PrivateKey: "a4087b340c1770b70cf5192413c6d7d7dbfc0a9db09b167965f133d672412c75",
	}
	autographDummyRsaJSON, _ := json.Marshal(autographDummyRsaConfig)

	// A signer that both alice and bob can access
	var autographAppKey2Config = signer.SanitizedConfig{
		ID:         "appkey2",
		Type:       "contentsignature",
		Mode:       "p256ecdsa",
		PublicKey:  "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEMdzAsqkWQiP8Fo89qTleJcuEjBtp2c6z16sC7BAS5KXvUGghURYq3utZw8En6Ik/4Om8c7EW/+EO+EkHShhgdA==",
		PrivateKey: "e41a1fb2466a1ba6c690ca6e3030c157745cfebe3c650f60c4c807ad781dd979",
	}
	autographAppKey2JSON, _ := json.Marshal(autographAppKey2Config)

	var testcases = []HandlerTestCase{
		{
			name:            "invalid method POST returns 405",
			method:          "POST",
			url:             "http://foo.bar/config/dummyrsa",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "POST method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "invalid method PUT returns 405",
			method:          "PUT",
			url:             "http://foo.bar/config/dummyrsa",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "PUT method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "invalid method OPTIONS returns 405",
			method:          "OPTIONS",
			url:             "http://foo.bar/config/dummyrsa",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "OPTIONS method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "invalid method HEAD returns 405",
			method:          "HEAD",
			url:             "http://foo.bar/config/dummyrsa",
			nilBody:         true,
			expectedStatus:  http.StatusMethodNotAllowed,
			expectedBody:    "HEAD method not allowed; endpoint accepts GET only\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with empty body returns 200",
			method:          "GET",
			url:             "http://foo.bar/config/dummyrsa",
			urlRouteVars:    map[string]string{"keyid": "dummyrsa"},
			nilBody:         false,
			body:            "",
			authorizeID:     "alice",
			expectedStatus:  http.StatusOK,
			expectedBody:    string(autographDummyRsaJSON),
			expectedHeaders: http.Header{"Content-Type": []string{"application/json"}},
		},
		{
			name:            "GET with non-empty body returns 400",
			method:          "GET",
			url:             "http://foo.bar/config/dummyrsa",
			urlRouteVars:    map[string]string{"keyid": "dummyrsa"},
			nilBody:         false,
			body:            "foobar/---",
			expectedStatus:  http.StatusBadRequest,
			expectedBody:    "endpoint received unexpected request body\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with misconfigured keyid route param returns 500",
			method:          "GET",
			url:             "http://foo.bar/config/dummyrsa",
			urlRouteVars:    map[string]string{}, // missing keyid
			nilBody:         true,
			expectedStatus:  http.StatusInternalServerError,
			expectedBody:    "route is improperly configured\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET missing Authorization header returns 401",
			method:          "GET",
			url:             "http://foo.bar/config/dummyrsa",
			urlRouteVars:    map[string]string{"keyid": "dummyrsa"},
			nilBody:         true,
			expectedStatus:  http.StatusUnauthorized,
			expectedBody:    "authorization verification failed: missing Authorization header\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with invalid Authorization header returns 401",
			method:          "GET",
			url:             "http://foo.bar/config/dummyrsa",
			urlRouteVars:    map[string]string{"keyid": "dummyrsa"},
			headers:         &http.Header{"Authorization": []string{`Hawk id="dh37fgj492je", ts="1353832234", nonce="j4h3g2", ext="some-app-ext-data", mac="6R4rV5iE+NPoym+WwjeHzjAGXUtLNIxmo1vpMofpLAE="`}},
			nilBody:         true,
			expectedStatus:  http.StatusUnauthorized,
			expectedBody:    "authorization verification failed: hawk: credential error with id dh37fgj492je and app : unknown id\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with invalid auth id url param returns 400",
			method:          "GET",
			url:             "http://foo.bar/config/",
			urlRouteVars:    map[string]string{"keyid": ""},
			nilBody:         true,
			expectedStatus:  http.StatusBadRequest,
			expectedBody:    "keyid in URL path '' is invalid, it must match ^[a-zA-Z0-9-_]{1,64}$\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with auth returns 404 for unknown keyid",
			method:          "GET",
			url:             "http://foo.bar/config/unknown",
			urlRouteVars:    map[string]string{"keyid": "unknown"},
			nilBody:         true,
			authorizeID:     "alice",
			expectedStatus:  http.StatusNotFound,
			expectedBody:    "keyid unknown was not found for user alice\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with auth returns 404 for mismatched auth ids (bob cannot get alice's keyids)",
			method:          "GET",
			url:             "http://foo.bar/config/dummyrsa",
			urlRouteVars:    map[string]string{"keyid": "dummyrsa"},
			nilBody:         true,
			authorizeID:     "bob",
			expectedStatus:  http.StatusNotFound,
			expectedBody:    "keyid dummyrsa was not found for user bob\r\nrequest-id: -\n",
			expectedHeaders: http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		},
		{
			name:            "GET with empty body returns 200 (bob can fetch their own keyids)",
			method:          "GET",
			url:             "http://foo.bar/config/appkey2",
			urlRouteVars:    map[string]string{"keyid": "appkey2"},
			nilBody:         false,
			body:            "",
			authorizeID:     "bob",
			expectedStatus:  http.StatusOK,
			expectedBody:    string(autographAppKey2JSON),
			expectedHeaders: http.Header{"Content-Type": []string{"application/json"}},
		},
	}

	for _, testcase := range testcases {
		testcase.Run(t, ag.handleGetConfig)
	}
}

func getAuthHeader(req *http.Request, user, token string, hash func() hash.Hash, ext, contenttype string, payload []byte) string {
	auth := hawk.NewRequestAuth(req,
		&hawk.Credentials{
			ID:   user,
			Key:  token,
			Hash: hash},
		0)
	auth.Ext = ext
	payloadhash := auth.PayloadHash(contenttype)
	payloadhash.Write(payload)
	auth.SetHash(payloadhash)
	return auth.RequestHeader()
}

func verifyXPISignature(input, sig string) error {
	rawInput, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		return err
	}
	pkcs7Sig, err := xpi.Unmarshal(sig, []byte(rawInput))
	if err != nil {
		log.Fatal(err)
	}
	return pkcs7Sig.VerifyWithChain(nil)
}

func verifyAPKManifestSignature(input, sig string) error {
	rawInput, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		return err
	}
	pkcs7Sig, err := apk2.Unmarshal(sig, []byte(rawInput))
	if err != nil {
		log.Fatal(err)
	}
	return pkcs7Sig.Verify()
}

func verifyAPKSignature(signedAPK []byte) error {
	zipReader := bytes.NewReader(signedAPK)
	r, err := zip.NewReader(zipReader, int64(len(signedAPK)))
	if err != nil {
		return err
	}
	var (
		sigstr  string
		sigdata []byte
	)
	for _, f := range r.File {
		switch f.Name {
		case "META-INF/SIGNATURE.SF",
			"META-INF/APK2_TES.SF",
			"META-INF/APK2_LEG.SF",
			"META-INF/APK2_APK.SF":
			rc, err := f.Open()
			defer rc.Close()
			if err != nil {
				return err
			}
			sigdata, err = ioutil.ReadAll(rc)
			if err != nil {
				return err
			}
		case "META-INF/SIGNATURE.RSA",
			"META-INF/APK2_TES.RSA",
			"META-INF/APK2_LEG.RSA",
			"META-INF/APK2_APK.EC":
			rc, err := f.Open()
			defer rc.Close()
			if err != nil {
				return err
			}
			rawsig, err := ioutil.ReadAll(rc)
			if err != nil {
				return err
			}
			sigstr = base64.StdEncoding.EncodeToString(rawsig)
		}
	}
	// convert string format back to signature
	sig, err := apk2.Unmarshal(sigstr, sigdata)
	if err != nil {
		return fmt.Errorf("failed to unmarshal signature: %v", err)
	}
	// verify signature on input data
	if sig.Verify() != nil {
		return fmt.Errorf("failed to verify apk signature: %v", sig.Verify())
	}
	return nil
}

func verifyMARSignature(b64Input, b64Sig, b64Key string, sigalg uint32) error {
	input, err := base64.StdEncoding.DecodeString(b64Input)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(b64Sig)
	if err != nil {
		return err
	}
	rawKey, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return err
	}
	key, err := x509.ParsePKIXPublicKey(rawKey)
	if err != nil {
		return err
	}
	return margo.VerifySignature(input, sig, sigalg, key)
}
