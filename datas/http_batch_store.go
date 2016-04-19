package datas

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/attic-labs/noms/chunks"
	"github.com/attic-labs/noms/constants"
	"github.com/attic-labs/noms/d"
	"github.com/attic-labs/noms/ref"
	"github.com/attic-labs/noms/types"
	"github.com/julienschmidt/httprouter"
)

const (
	httpChunkSinkConcurrency = 6
	writeBufferSize          = 1 << 12 // 4K
	readBufferSize           = 1 << 12 // 4K
)

// httpBatchStore implements types.BatchStore
type httpBatchStore struct {
	host          *url.URL
	httpClient    httpDoer
	auth          string
	readQueue     chan chunks.ReadRequest
	writeQueue    chan writeRequest
	flushChan     chan struct{}
	finishedChan  chan struct{}
	rateLimit     chan struct{}
	requestWg     *sync.WaitGroup
	workerWg      *sync.WaitGroup
	unwrittenPuts *unwrittenPutCache
}

func newHTTPBatchStore(baseURL, auth string) *httpBatchStore {
	u, err := url.Parse(baseURL)
	d.Exp.NoError(err)
	d.Exp.True(u.Scheme == "http" || u.Scheme == "https")
	buffSink := &httpBatchStore{
		host:          u,
		httpClient:    makeHTTPClient(httpChunkSinkConcurrency),
		auth:          auth,
		readQueue:     make(chan chunks.ReadRequest, readBufferSize),
		writeQueue:    make(chan writeRequest, writeBufferSize),
		flushChan:     make(chan struct{}),
		finishedChan:  make(chan struct{}),
		rateLimit:     make(chan struct{}, httpChunkSinkConcurrency),
		requestWg:     &sync.WaitGroup{},
		workerWg:      &sync.WaitGroup{},
		unwrittenPuts: newUnwrittenPutCache(),
	}
	buffSink.batchGetRequests()
	buffSink.batchPutRequests()
	return buffSink
}

type httpDoer interface {
	Do(req *http.Request) (resp *http.Response, err error)
}

type writeRequest struct {
	c     chunks.Chunk
	hints types.Hints
}

// Use a custom http client rather than http.DefaultClient. We limit ourselves to a maximum of |requestLimit| concurrent http requests, the custom httpClient ups the maxIdleConnsPerHost value so that one connection stays open for each concurrent request.
func makeHTTPClient(requestLimit int) *http.Client {
	t := http.Transport(*http.DefaultTransport.(*http.Transport))
	t.MaxIdleConnsPerHost = requestLimit

	return &http.Client{
		Transport: &t,
		Timeout:   time.Duration(30) * time.Second,
	}
}

func (bhcs *httpBatchStore) Flush() {
	bhcs.flushChan <- struct{}{}
	bhcs.requestWg.Wait()
	return
}

func (bhcs *httpBatchStore) Close() (e error) {
	close(bhcs.finishedChan)
	bhcs.requestWg.Wait()
	bhcs.workerWg.Wait()

	close(bhcs.flushChan)
	close(bhcs.writeQueue)
	close(bhcs.rateLimit)
	return
}

func (bhcs *httpBatchStore) Get(r ref.Ref) chunks.Chunk {
	pending := bhcs.unwrittenPuts.Get(r)
	if !pending.IsEmpty() {
		return pending
	}

	ch := make(chan chunks.Chunk)
	bhcs.requestWg.Add(1)
	bhcs.readQueue <- chunks.NewGetRequest(r, ch)
	return <-ch
}

func (bhcs *httpBatchStore) batchGetRequests() {
	bhcs.workerWg.Add(1)
	go func() {
		defer bhcs.workerWg.Done()

		for done := false; !done; {
			select {
			case req := <-bhcs.readQueue:
				bhcs.sendGetRequests(req)
			case <-bhcs.finishedChan:
				done = true
			}
			// Drain the readQueue before returning
			select {
			case req := <-bhcs.readQueue:
				bhcs.sendGetRequests(req)
			default:
				//drained!
			}
		}
	}()
}

func (bhcs *httpBatchStore) sendGetRequests(req chunks.ReadRequest) {
	batch := chunks.ReadBatch{}
	refs := types.Hints{}

	addReq := func(req chunks.ReadRequest) {
		r := req.Ref()
		batch[r] = append(batch[r], req.Outstanding())
		refs[r] = struct{}{}
	}

	addReq(req)
	for drained := false; !drained && len(refs) < readBufferSize; {
		select {
		case req := <-bhcs.readQueue:
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

		bhcs.getRefs(refs, &batch)
		<-bhcs.rateLimit
	}()
}

func (bhcs *httpBatchStore) getRefs(refs types.Hints, cs chunks.ChunkSink) {
	// POST http://<host>/getRefs/. Post body: ref=sha1---&ref=sha1---& Response will be chunk data if present, 404 if absent.
	u := *bhcs.host
	u.Path = httprouter.CleanPath(bhcs.host.Path + constants.GetRefsPath)

	req := newRequest("POST", bhcs.auth, u.String(), buildGetRefsRequest(refs), http.Header{
		"Accept-Encoding": {"gzip"},
		"Content-Type":    {"application/x-www-form-urlencoded"},
	})

	res, err := bhcs.httpClient.Do(req)
	d.Chk.NoError(err)
	defer closeResponse(res)
	d.Chk.Equal(http.StatusOK, res.StatusCode, "Unexpected response: %s", http.StatusText(res.StatusCode))

	reader := res.Body
	if strings.Contains(res.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(reader)
		d.Chk.NoError(err)
		defer gr.Close()
		reader = gr
	}

	rl := make(chan struct{}, 1) // Rate limit to 1 because there are already N goroutines waiting on responses, all we need to do is send the Chunks back through their channels.
	chunks.Deserialize(reader, cs, rl)
}

func (bhcs *httpBatchStore) SchedulePut(c chunks.Chunk, hints types.Hints) {
	if !bhcs.unwrittenPuts.Add(c) {
		return
	}

	bhcs.requestWg.Add(1)
	bhcs.writeQueue <- writeRequest{c, hints}
}

func (bhcs *httpBatchStore) batchPutRequests() {
	bhcs.workerWg.Add(1)
	go func() {
		defer bhcs.workerWg.Done()

		numChunks := 0
		hints := types.Hints{}
		buf := makeBuffer()
		gw := gzip.NewWriter(buf)
		sz := chunks.NewSerializer(gw)
		handleRequest := func(wr writeRequest) {
			numChunks++
			sz.Put(wr.c)
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
						d.Chk.NoError(sz.Close())
						d.Chk.NoError(gw.Close())
						_, err := buf.Seek(0, 0)
						d.Chk.NoError(err, "Could not reset filesystem buffer to offset 0.")
						bhcs.sendWriteRequests(buf, numChunks, hints) // Takes ownership of buf, hints

						numChunks = 0
						hints = types.Hints{}
						buf = makeBuffer()
						gw = gzip.NewWriter(buf)
						sz = chunks.NewSerializer(gw)
					}
				}
			}
		}
	}()
}

func makeBuffer() *os.File {
	f, err := ioutil.TempFile("", "http_hinted_chunk_store_")
	d.Chk.NoError(err, "Cannot create filesystem buffer for Chunks.")
	return f
}

func (bhcs *httpBatchStore) sendWriteRequests(serializedChunks *os.File, numChunks int, hints types.Hints) {
	bhcs.rateLimit <- struct{}{}
	go func() {
		defer func() {
			bhcs.unwrittenPuts = newUnwrittenPutCache()
			bhcs.requestWg.Add(-numChunks)
			d.Chk.NoError(serializedChunks.Close(), "Cannot close filesystem buffer.")
			d.Chk.NoError(os.Remove(serializedChunks.Name()), "Cannot remove filesystem buffer.")
		}()

		body := buildWriteValueRequest(serializedChunks, hints)

		url := *bhcs.host
		url.Path = httprouter.CleanPath(bhcs.host.Path + constants.WriteValuePath)
		req := newRequest("POST", bhcs.auth, url.String(), body, http.Header{
			"Content-Encoding": {"gzip"},
			"Content-Type":     {"application/octet-stream"},
		})

		res, err := bhcs.httpClient.Do(req)
		d.Exp.NoError(err)
		defer closeResponse(res)
		d.Exp.Equal(http.StatusCreated, res.StatusCode, "Unexpected response: %s", formatErrorResponse(res))
		<-bhcs.rateLimit
	}()
}

func (bhcs *httpBatchStore) Root() ref.Ref {
	// GET http://<host>/root. Response will be ref of root.
	res := bhcs.requestRoot("GET", ref.Ref{}, ref.Ref{})
	defer closeResponse(res)

	d.Chk.Equal(http.StatusOK, res.StatusCode, "Unexpected response: %s", http.StatusText(res.StatusCode))
	data, err := ioutil.ReadAll(res.Body)
	d.Chk.NoError(err)
	return ref.Parse(string(data))
}

func (bhcs *httpBatchStore) UpdateRoot(current, last ref.Ref) bool {
	// POST http://<host>/root?current=<ref>&last=<ref>. Response will be 200 on success, 409 if current is outdated.
	bhcs.Flush()

	res := bhcs.requestRoot("POST", current, last)
	defer closeResponse(res)

	d.Chk.True(res.StatusCode == http.StatusOK || res.StatusCode == http.StatusConflict, "Unexpected response: %s", http.StatusText(res.StatusCode))
	return res.StatusCode == http.StatusOK
}

func (bhcs *httpBatchStore) requestRoot(method string, current, last ref.Ref) *http.Response {
	u := *bhcs.host
	u.Path = httprouter.CleanPath(bhcs.host.Path + constants.RootPath)
	if method == "POST" {
		d.Exp.False(current.IsEmpty())
		params := url.Values{}
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
	d.Chk.Equal(0, len(data), string(data))
	return res.Body.Close()
}