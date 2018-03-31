package bulkhttpclient

import (
	"net/http"
	"context"
	"time"
	"fmt"
	"errors"
	"io/ioutil"
	"bytes"
)

//HTTPClient ...
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

//Request ..
type Request interface {
	Add(*http.Request) Request
}

//BulkClient ...
type BulkClient struct {
	httpclient HTTPClient
	timeout    time.Duration
}

//RoundTrip ...
type RoundTrip struct {
	requests  []*http.Request
	responses []*http.Response
	errors    []error
}

//ErrNoRequests ...
var ErrNoRequests = errors.New("no requests provided")

//ErrRequestIgnored ...
var ErrRequestIgnored = errors.New("request ignored")

type requestParcel struct {
	request *http.Request
	index    int
}

type responseParcel struct {
	req      *http.Request // this is required to recreate a http.Response with a new http.Request without a context
	response *http.Response
	err      error
	index    int
}

//NewBulkHTTPClient ...
func NewBulkHTTPClient(client HTTPClient, timeout time.Duration) *BulkClient {
	return &BulkClient{
		httpclient: client,
		timeout:    timeout,
	}
}

//NewBulkRequest ...
func NewBulkRequest() *RoundTrip {
	return &RoundTrip{
		requests:  []*http.Request{},
		responses: []*http.Response{},
	}
}

//AddRequest ...
func (r *RoundTrip) AddRequest(request *http.Request) *RoundTrip {
	r.requests = append(r.requests, request)
	return r
}

//Do ...
func (cl *BulkClient) Do(bulkRequest *RoundTrip) ([]*http.Response, []error) {
	noOfRequests := len(bulkRequest.requests)
	if noOfRequests == 0 {
		return nil, []error{ErrNoRequests}
	}

	bulkRequest.responses = make([]*http.Response, noOfRequests)
	bulkRequest.errors    = make([]error, noOfRequests)

	requestList        := make(chan requestParcel)
	recievedResponses  := make(chan roundTripParcel)
	processedResponses := make(chan roundTripParcel)

	for nWorker := 0; nWorker < 10; nWorker++ {
		go cl.fireRequests(requestList, recievedResponses)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cl.timeout)
	defer cancel()

	for mWorker := 0; mWorker < 10; mWorker++ {
		go cl.processRequests(ctx, recievedResponses, processedResponses)
	}

	for index, req := range bulkRequest.requests {
		bulkRequest.requests[index] = req.WithContext(ctx)
		reqParcel := requestParcel{
			request: bulkRequest.requests[index],
			index: index,
		}

		requestList <- reqParcel
	}

	return cl.completionListener(ctx, bulkRequest, processedResponses)
}

//CloseAllResponses ...
func (r *RoundTrip) CloseAllResponses() {
	for _, response := range r.responses {
		if response != nil {
			response.Body.Close()
		}
	}
}

func (cl *BulkClient) completionListener(ctx context.Context, bulkRequest *RoundTrip, processedResponses <-chan responseParcel) ([]*http.Response, []error) {
	LOOP:
	for done := 0; done < len(bulkRequest.requests); {
		select {
		case <-ctx.Done():
			break LOOP
		case resParcel := <-processedResponses:
			if resParcel.err != nil {
				bulkRequest.updateErrorForIndex(resParcel.err, resParcel.index)
			} else {
				bulkRequest.updateResponseForIndex(resParcel.response, resParcel.index)
			}

			done++
		}
	}

	bulkRequest.addRequestIgnoredErrors()
	return bulkRequest.responses, bulkRequest.errors
}

func (r *RoundTrip) addRequestIgnoredErrors() {
	for i, response := range r.responses {
		if response == nil && r.errors[i] == nil {
			r.errors[i] = ErrRequestIgnored
		}
	}
}

func (r *RoundTrip) updateResponseForIndex(response *http.Response, index int) *RoundTrip {
	r.responses[index] = response
	r.errors[index] = nil
	return r
}

func (r *RoundTrip) updateErrorForIndex(err error, index int) *RoundTrip {
	r.errors[index] = err
	r.responses[index] = nil
	return r
}

func (cl *BulkClient) fireRequests(reqList <-chan requestParcel, receivedResponses chan<- responseParcel) {
	LOOP:
	for {
		select {
		case reqParcel, isChanOpen := <-reqList:
			if !isChanOpen {
				break LOOP
			}

			receivedResponses <- cl.executeRequest(reqParcel)
		}
	}
}

func (cl *BulkClient) executeRequest(reqParcel requestParcel) responseParcel {
	resp, err := cl.httpclient.Do(reqParcel.request)

	return responseParcel{
		req:      reqParcel.request,
		response: resp,
		err:      err,
		index:    reqParcel.index,
	}
}

func (cl *BulkClient) processRequests(ctx context.Context, resList <-chan responseParcel, processedResponses chan<- responseParcel) {
	LOOP:
	for {
		select {
		case resParcel, isChanOpen := <-resList:
			if !isChanOpen {
				break LOOP
			}

			processedResponses <- cl.parseResponse(ctx, resParcel)
		}
	}
}

func (cl *BulkClient) parseResponse(ctx context.Context, res responseParcel) responseParcel {
	defer func() {
		if res.response != nil {
			res.response.Body.Close()
		}
	}()

	if res.err != nil && (ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded) {
		return responseParcel{err: ErrRequestIgnored, index: res.index}
	}

	if res.err != nil {
		return responseParcel{err: fmt.Errorf("http client error: %s", res.err), index: res.index}
	}

	if res.response == nil {
		return responseParcel{err: errors.New("no response received"), index: res.index}
	}

	bs, err := ioutil.ReadAll(res.response.Body)
	if err != nil {
		return responseParcel{err: fmt.Errorf("error while reading response body: %s", err), index: res.index}
	}

	body := ioutil.NopCloser(bytes.NewReader(bs))

	newResponse := http.Response{
		Body:       body,
		StatusCode: res.response.StatusCode,
		Status:     res.response.Status,
		Header:     res.response.Header,
		Request:    res.req.WithContext(context.Background()),
	}

	result := responseParcel{
		response: &newResponse,
		err:      err,
		index:    res.index,
	}

	return result
}
