// Copyright 2016 The Noms Authors. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package datas

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/attic-labs/noms/go/chunks"
	"github.com/attic-labs/noms/go/constants"
	"github.com/attic-labs/noms/go/d"
	"github.com/attic-labs/noms/go/hash"
	"github.com/attic-labs/noms/go/types"
	"github.com/julienschmidt/httprouter"
)

const (
	httpChunkSinkConcurrency = 6
	writeBufferSize          = 1 << 12 // 4K
	readBufferSize           = 1 << 12 // 4K

	httpStatusTooManyRequests = 429 // This is new in Go 1.6. Once the builders have that, use it.
)

// httpBatchStore implements types.BatchStore
type httpBatchStore struct {
	host          *url.URL
	httpClient    httpDoer
	auth          string
	getQueue      chan chunks.ReadRequest
	hasQueue      chan chunks.ReadRequest
	writeQueue    chan writeRequest
	flushChan     chan struct{}
	finishedChan  chan struct{}
	rateLimit     chan struct{}
	requestWg     *sync.WaitGroup
	workerWg      *sync.WaitGroup
	unwrittenPuts *orderedChunkCache
}

func newHTTPBatchStore(baseURL, auth string) *httpBatchStore {
	u, err := url.Parse(baseURL)
	d.Exp.NoError(err)
	d.Exp.True(u.Scheme == "http" || u.Scheme == "https")
	buffSink := &httpBatchStore{
		host:          u,
		httpClient:    makeHTTPClient(httpChunkSinkConcurrency),
		auth:          auth,
		getQueue:      make(chan chunks.ReadRequest, readBufferSize),
		hasQueue:      make(chan chunks.ReadRequest, readBufferSize),
		writeQueue:    make(chan writeRequest, writeBufferSize),
		flushChan:     make(chan struct{}),
		finishedChan:  make(chan struct{}),
		rateLimit:     make(chan struct{}, httpChunkSinkConcurrency),
		requestWg:     &sync.WaitGroup{},
		workerWg:      &sync.WaitGroup{},
		unwrittenPuts: newOrderedChunkCache(),
	}
	buffSink.batchGetRequests()
	buffSink.batchHasRequests()
	buffSink.batchPutRequests()
	return buffSink
}

type httpDoer interface {
	Do(req *http.Request) (resp *http.Response, err error)
}

type writeRequest struct {
	hash      hash.Hash
	hints     types.Hints
	justHints bool
}

// Use a custom http client rather than http.DefaultClient. We limit ourselves to a maximum of |requestLimit| concurrent http requests, the custom httpClient ups the maxIdleConnsPerHost value so that one connection stays open for each concurrent request.
func makeHTTPClient(requestLimit int) *http.Client {
	t := http.Transport(*http.DefaultTransport.(*http.Transport))
	t.MaxIdleConnsPerHost = requestLimit
	// This sets, essentially, an idle-timeout. The timer starts counting AFTER the client has finished sending the entire request to the server. As soon as the client receives the server's response headers, the timeout is canceled.
	t.ResponseHeaderTimeout = time.Duration(2) * time.Minute

	return &http.Client{Transport: &t}
}

func (bhcs *httpBatchStore) Flush() {
	bhcs.flushChan <- struct{}{}
	bhcs.requestWg.Wait()
	return
}

func (bhcs *httpBatchStore) Close() (e error) {
	close(bhcs.finishedChan)
	bhcs.unwrittenPuts.Destroy()
	bhcs.requestWg.Wait()
	bhcs.workerWg.Wait()

	close(bhcs.flushChan)
	close(bhcs.getQueue)
	close(bhcs.hasQueue)
	close(bhcs.writeQueue)
	close(bhcs.rateLimit)
	return
}

func (bhcs *httpBatchStore) Get(h hash.Hash) chunks.Chunk {
	if pending := bhcs.unwrittenPuts.Get(h); !pending.IsEmpty() {
		return pending
	}

	ch := make(chan chunks.Chunk)
	bhcs.requestWg.Add(1)
	bhcs.getQueue <- chunks.NewGetRequest(h, ch)
	return <-ch
}

func (bhcs *httpBatchStore) batchGetRequests() {
	bhcs.batchReadRequests(bhcs.getQueue, bhcs.getRefs)
}

func (bhcs *httpBatchStore) Has(h hash.Hash) bool {
	if bhcs.unwrittenPuts.has(h) {
		return true
	}

	ch := make(chan bool)
	bhcs.requestWg.Add(1)
	bhcs.hasQueue <- chunks.NewHasRequest(h, ch)
	return <-ch
}

func (bhcs *httpBatchStore) batchHasRequests() {
	bhcs.batchReadRequests(bhcs.hasQueue, bhcs.hasRefs)
}

type batchGetter func(hashes hashSet, batch chunks.ReadBatch)

func (bhcs *httpBatchStore) batchReadRequests(queue <-chan chunks.ReadRequest, getter batchGetter) {
	bhcs.workerWg.Add(1)
	go func() {
		defer bhcs.workerWg.Done()

		for done := false; !done; {
			select {
			case req := <-queue:
				bhcs.sendReadRequests(req, queue, getter)
			case <-bhcs.finishedChan:
				done = true
			}
			// Drain queue before returning
			select {
			case req := <-queue:
				bhcs.sendReadRequests(req, queue, getter)
			default:
				//drained!
			}
		}
	}()
}

func (bhcs *httpBatchStore) sendReadRequests(req chunks.ReadRequest, queue <-chan chunks.ReadRequest, getter batchGetter) {
	batch := chunks.ReadBatch{}
	hashes := hashSet{}

	addReq := func(req chunks.ReadRequest) {
		hash := req.Hash()
		batch[hash] = append(batch[hash], req.Outstanding())
		hashes.Insert(hash)
	}

	addReq(req)
	for drained := false; !drained && len(hashes) < readBufferSize; {
		select {
		case req := <-queue:
			addReq(req)
		default:
			drained = true
		}
	}

	fullBatchSize := len(batch)
	bhcs.rateLimit <- struct{}{}
	go func() {
		defer func() {
			bhcs.requestWg.Add(-fullBatchSize)
			batch.Close()
		}()

		getter(hashes, batch)
		<-bhcs.rateLimit
	}()
}

func (bhcs *httpBatchStore) getRefs(hashes hashSet, batch chunks.ReadBatch) {
	// POST http://<host>/getRefs/. Post body: ref=sha1---&ref=sha1---& Response will be chunk data if present, 404 if absent.
	u := *bhcs.host
	u.Path = httprouter.CleanPath(bhcs.host.Path + constants.GetRefsPath)

	req := newRequest("POST", bhcs.auth, u.String(), buildHashesRequest(hashes), http.Header{
		"Accept-Encoding": {"gzip"},
		"Content-Type":    {"application/x-www-form-urlencoded"},
	})

	res, err := bhcs.httpClient.Do(req)
	d.Chk.NoError(err)
	defer closeResponse(res)
	d.Chk.True(http.StatusOK == res.StatusCode, "Unexpected response: %s", http.StatusText(res.StatusCode))

	reader := resBodyReader(res)
	defer reader.Close()

	rl := make(chan struct{}, 1) // Rate limit to 1 because there are already N goroutines waiting on responses, all we need to do is send the Chunks back through their channels.
	chunks.Deserialize(reader, &batch, rl)
}

func (bhcs *httpBatchStore) hasRefs(hashes hashSet, batch chunks.ReadBatch) {
	// POST http://<host>/hasRefs/. Post body: ref=sha1---&ref=sha1---& Response will be text of lines containing "|ref| |bool|".
	u := *bhcs.host
	u.Path = httprouter.CleanPath(bhcs.host.Path + constants.HasRefsPath)

	req := newRequest("POST", bhcs.auth, u.String(), buildHashesRequest(hashes), http.Header{
		"Accept-Encoding": {"gzip"},
		"Content-Type":    {"application/x-www-form-urlencoded"},
	})

	res, err := bhcs.httpClient.Do(req)
	d.Chk.NoError(err)
	defer closeResponse(res)
	d.Chk.True(http.StatusOK == res.StatusCode, "Unexpected response: %s", http.StatusText(res.StatusCode))

	reader := resBodyReader(res)
	defer reader.Close()

	scanner := bufio.NewScanner(reader)
	scanner.Split(bufio.ScanWords)
	for scanner.Scan() {
		h := hash.Parse(scanner.Text())
		d.Chk.True(scanner.Scan())
		if scanner.Text() == "true" {
			for _, outstanding := range batch[h] {
				// This is a little gross, but OutstandingHas.Satisfy() expects a chunk. It ignores it, though, and just sends 'true' over the channel it's holding.
				outstanding.Satisfy(chunks.EmptyChunk)
			}
		} else {
			for _, outstanding := range batch[h] {
				outstanding.Fail()
			}
		}
		delete(batch, h)
	}
}

func resBodyReader(res *http.Response) (reader io.ReadCloser) {
	reader = res.Body
	if strings.Contains(res.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(reader)
		d.Chk.NoError(err)
		reader = gr
	}
	return
}

func (bhcs *httpBatchStore) SchedulePut(c chunks.Chunk, refHeight uint64, hints types.Hints) {
	if !bhcs.unwrittenPuts.Insert(c, refHeight) {
		return
	}

	bhcs.requestWg.Add(1)
	bhcs.writeQueue <- writeRequest{c.Hash(), hints, false}
}

func (bhcs *httpBatchStore) AddHints(hints types.Hints) {
	bhcs.writeQueue <- writeRequest{hash.Hash{}, hints, true}
}

func (bhcs *httpBatchStore) batchPutRequests() {
	bhcs.workerWg.Add(1)
	go func() {
		defer bhcs.workerWg.Done()

		hints := types.Hints{}
		hashes := hashSet{}
		handleRequest := func(wr writeRequest) {
			if !wr.justHints {
				if hashes.Has(wr.hash) {
					bhcs.requestWg.Done() // Already have a put enqueued for wr.hash.
				} else {
					hashes.Insert(wr.hash)
				}
			}
			for hint := range wr.hints {
				hints[hint] = struct{}{}
			}
		}
		for done := false; !done; {
			drainAndSend := false
			select {
			case wr := <-bhcs.writeQueue:
				handleRequest(wr)
			case <-bhcs.flushChan:
				drainAndSend = true
			case <-bhcs.finishedChan:
				drainAndSend = true
				done = true
			}

			if drainAndSend {
				for drained := false; !drained; {
					select {
					case wr := <-bhcs.writeQueue:
						handleRequest(wr)
					default:
						drained = true
						bhcs.sendWriteRequests(hashes, hints) // Takes ownership of hashes, hints
						hints = types.Hints{}
						hashes = hashSet{}
					}
				}
			}
		}
	}()
}

func (bhcs *httpBatchStore) sendWriteRequests(hashes hashSet, hints types.Hints) {
	if len(hashes) == 0 {
		return
	}
	bhcs.rateLimit <- struct{}{}
	go func() {
		defer func() {
			<-bhcs.rateLimit
			bhcs.unwrittenPuts.Clear(hashes)
			bhcs.requestWg.Add(-len(hashes))
		}()

		var res *http.Response
		var err error
		for tryAgain := true; tryAgain; {
			serializedChunks, pw := io.Pipe()
			errChan := make(chan error)
			go func() {
				gw := gzip.NewWriter(pw)
				err := bhcs.unwrittenPuts.ExtractChunks(hashes, gw)
				// The ordering of these is important. Close the gzipper to flush data to pw, then close pw so that the HTTP stack which is reading from serializedChunks knows it has everything, and only THEN block on errChan.
				gw.Close()
				pw.Close()
				errChan <- err
				close(errChan)
			}()
			body := buildWriteValueRequest(serializedChunks, hints)

			url := *bhcs.host
			url.Path = httprouter.CleanPath(bhcs.host.Path + constants.WriteValuePath)
			req := newRequest("POST", bhcs.auth, url.String(), body, http.Header{
				"Accept-Encoding":  {"gzip"},
				"Content-Encoding": {"gzip"},
				"Content-Type":     {"application/octet-stream"},
			})

			res, err = bhcs.httpClient.Do(req)
			d.Exp.NoError(err)
			d.Exp.NoError(<-errChan)
			defer closeResponse(res)

			if tryAgain = res.StatusCode == httpStatusTooManyRequests; tryAgain {
				reader := res.Body
				if strings.Contains(res.Header.Get("Content-Encoding"), "gzip") {
					gr, err := gzip.NewReader(reader)
					d.Exp.NoError(err)
					defer gr.Close()
					reader = gr
				}
				/*hashes :=*/ deserializeHashes(reader)
				// TODO: BUG 1259 Since the client must currently send all chunks in one batch, the only thing to do in response to backpressure is send EVERYTHING again. Once batching is again possible, this code should figure out how to resend the chunks indicated by hashes.
			}
		}

		d.Exp.True(http.StatusCreated == res.StatusCode, "Unexpected response: %s", formatErrorResponse(res))
	}()
}

func (bhcs *httpBatchStore) Root() hash.Hash {
	// GET http://<host>/root. Response will be ref of root.
	res := bhcs.requestRoot("GET", hash.Hash{}, hash.Hash{})
	defer closeResponse(res)

	d.Chk.True(http.StatusOK == res.StatusCode, "Unexpected response: %s", http.StatusText(res.StatusCode))
	data, err := ioutil.ReadAll(res.Body)
	d.Chk.NoError(err)
	return hash.Parse(string(data))
}

func (bhcs *httpBatchStore) UpdateRoot(current, last hash.Hash) bool {
	// POST http://<host>/root?current=<ref>&last=<ref>. Response will be 200 on success, 409 if current is outdated.
	bhcs.Flush()

	res := bhcs.requestRoot("POST", current, last)
	defer closeResponse(res)

	d.Chk.True(res.StatusCode == http.StatusOK || res.StatusCode == http.StatusConflict, "Unexpected response: %s", http.StatusText(res.StatusCode))
	return res.StatusCode == http.StatusOK
}

func (bhcs *httpBatchStore) requestRoot(method string, current, last hash.Hash) *http.Response {
	u := *bhcs.host
	u.Path = httprouter.CleanPath(bhcs.host.Path + constants.RootPath)
	if method == "POST" {
		d.Exp.False(current.IsEmpty())
		params := u.Query()
		params.Add("last", last.String())
		params.Add("current", current.String())
		u.RawQuery = params.Encode()
	}

	req := newRequest(method, bhcs.auth, u.String(), nil, nil)

	res, err := bhcs.httpClient.Do(req)
	d.Chk.NoError(err)

	return res
}

func newRequest(method, auth, url string, body io.Reader, header http.Header) *http.Request {
	req, err := http.NewRequest(method, url, body)
	d.Chk.NoError(err)
	for k, vals := range header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

func formatErrorResponse(res *http.Response) string {
	data, err := ioutil.ReadAll(res.Body)
	d.Chk.NoError(err)
	return fmt.Sprintf("%s:\n%s\n", res.Status, data)
}

// In order for keep alive to work we must read to EOF on every response. We may want to add a timeout so that a server that left its connection open can't cause all of ports to be eaten up.
func closeResponse(res *http.Response) error {
	data, err := ioutil.ReadAll(res.Body)
	d.Chk.NoError(err)
	d.Chk.True(0 == len(data), string(data))
	return res.Body.Close()
}