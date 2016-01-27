// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor: Julien Vehent jvehent@mozilla.com [:ulfr]

package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/hashicorp/golang-lru"
)

// A autographer signs input data with a private key
type autographer struct {
	signers     []signer
	auths       map[string]authorization
	signerIndex map[string]int
	nonces      *lru.Cache
}

func newAutographer(cachesize int) (a *autographer, err error) {
	a = new(autographer)
	a.nonces, err = lru.New(cachesize)
	return
}

// addSigners initializes each signer specified in the configuration by parsing
// and loading their private keys. The signers are then copied over to the
// autographer handler.
func (a *autographer) addSigners(signers []signer) {
	for _, signer := range signers {
		err := signer.init()
		if err != nil {
			panic(err)
		}
		a.signers = append(a.signers, signer)
	}
}

// addAuthorizations reads a list of authorizations from the configuration and
// stores them into the autographer handler as a map indexed by user id, for fast lookup.
func (a *autographer) addAuthorizations(auths []authorization) {
	a.auths = make(map[string]authorization)
	for _, auth := range auths {
		if _, ok := a.auths[auth.ID]; ok {
			panic("authorization id '" + auth.ID + "' already defined, duplicates are not permitted")
		}
		a.auths[auth.ID] = auth
	}
}

// makeSignerIndex creates a map of authorization IDs and signer IDs to
// quickly locate a signer based on the user requesting the signature.
func (a *autographer) makeSignerIndex() {
	a.signerIndex = make(map[string]int)
	// add an entry for each authid+signerid pair
	for _, auth := range a.auths {
		for _, sid := range auth.Signers {
			for pos, s := range a.signers {
				if sid == s.ID {
					log.Printf("Mapping auth id %q and signer id %q to signer %d", auth.ID, s.ID, pos)
					tag := auth.ID + "+" + s.ID
					a.signerIndex[tag] = pos
				}
			}
		}
	}
	// add a fallback entry with just the authid, to use when no signerid
	// is specified in the signing request. This entry maps to the first
	// authorized signer
	for _, auth := range a.auths {
		if len(auth.Signers) < 1 {
			continue
		}
		for pos, signer := range a.signers {
			if auth.Signers[0] == signer.ID {
				log.Printf("Mapping auth id %q to default signer %d", auth.ID, pos)
				tag := auth.ID + "+"
				a.signerIndex[tag] = pos
				break
			}
		}
	}
}

// handleSignature endpoint accepts a list of signature requests in a HAWK authenticated POST request
// and calls the signers to generate signature responses.
func (a *autographer) handleSignature(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		httpError(w, http.StatusMethodNotAllowed, "%s method not allowed; endpoint accepts POST only", r.Method)
		return
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read request body: %s", err)
		return
	}
	userid, authorized, err := a.authorize(r, body)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "authorization verification failed: %v", err)
		return
	}
	if !authorized {
		httpError(w, http.StatusUnauthorized, "request is not authorized; provide a valid HAWK authorization")
		return
	}
	var sigreqs []signaturerequest
	err = json.Unmarshal(body, &sigreqs)
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to parse request body: %v", err)
		return
	}
	sigresps := make([]signatureresponse, len(sigreqs))
	for i, sigreq := range sigreqs {
		hash, err := getInputHash(sigreq)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%v", err)
			return
		}
		signerID, err := a.getSignerID(userid, sigreq.KeyID)
		if err != nil || signerID < 0 {
			httpError(w, http.StatusInternalServerError, "could not get signer: %v", err)
			return
		}
		rawsig, err := a.signers[signerID].sign(hash)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "signing failed with error: %v", err)
			return
		}
		sigresps[i].Signatures = append(sigresps[i].Signatures, signaturedata{
			Encoding:  "b64url",
			Signature: rawsig.toBase64Url(),
			Hash:      "sha384",
		})
		sigresps[i].Ref = id()
	}
	respdata, err := json.Marshal(sigresps)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "signing failed with error: %v", err)
		return
	}
	log.Printf("signing operation succeeded:%s", respdata)
	w.WriteHeader(http.StatusCreated)
	w.Write(respdata)
}

// handleHeartbeat returns a simple message indicating that the API is alive and well
func (a *autographer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		httpError(w, http.StatusMethodNotAllowed, "%s method not allowed; endpoint accepts GET only", r.Method)
		return
	}
	w.Write([]byte("ohai"))
}
