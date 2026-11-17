package client

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// problemOf is r's refusal as the problem its body carries. A redirect, or
// a body that is no problem, such as a proxy's error page, reads as a
// problem holding only r's status, which is never retryable.
func problemOf(r reply) *apiv1.Problem {
	p := &apiv1.Problem{}
	mt, _, _ := mime.ParseMediaType(r.header.Get("Content-Type"))
	if r.status < 400 || mt != apiv1.MediaTypeProblem || json.Unmarshal(r.body, p) != nil {
		p = &apiv1.Problem{Title: http.StatusText(r.status)}
	}
	p.Status = r.status
	return p
}

// retryAfter is how long a retryable refusal asks the client to wait: the
// seconds its Retry-After names, at most a minute, or retryWait when it
// names none or a negative number.
//
// ponytail: Retry-After is read only as a number of seconds, the form the
// API sends; its HTTP-date form reads as absent. Parse the date too if a
// proxy in front of the API is seen sending one.
func retryAfter(h http.Header) time.Duration {
	s, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || s < 0 {
		return retryWait
	}
	return time.Duration(min(s, maxRetryAfter)) * time.Second
}

// maxRetryAfter is the longest Retry-After, in seconds, the client waits.
const maxRetryAfter = 60
