package dtrace

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ditrace/ditrace/metrics"
	"gopkg.in/olivere/elastic.v3"
)

const esDocType string = "trace"

// ESClientFactory is a factory to create ESClient
var ESClientFactory = newESClient

// ESClient is the interface for elasticsearch client
type ESClient interface {
	Bulk() ESBulkService
	NewBulkIndexRequest() ESBulkRequest
}

// ESBulkService is the interface for elasticsearch bulk service
type ESBulkService interface {
	Add(ESBulkRequest) ESBulkService
	Do() (*elastic.BulkResponse, error)
}

// ESBulkRequest is the interface for elasticsearch bulk request
type ESBulkRequest interface {
	Index(name string) ESBulkRequest
	Type(name string) ESBulkRequest
	Doc(doc interface{}) ESBulkRequest
}

// Document for elasticsearch
type Document struct {
	Fields map[string]interface{}
	Index  string
}

// GetESDocuments returns document for elasticsearch
func (trace *Trace) GetESDocuments() []*Document {
	var documents []*Document
	if len(trace.Roots) == 0 {
		trace.Roots[trace.Root.ID] = trace.Root
	}
	for rootSpanID, root := range trace.Roots {
		doc := &Document{
			Fields: make(map[string]interface{}),
		}

		timestamp := root.Timeline.get(trace.Timestamp, "cs", "sr")
		doc.Fields["timestamp"] = timestamp
		doc.Fields["id"] = trace.ID
		doc.Fields["system"] = root.System
		doc.Fields["duration"] = root.Duration()
		if len(trace.ProfileID) > 0 {
			doc.Fields["profileid"] = trace.ProfileID
		}
		doc.Index = fmt.Sprintf("traces-%s", timestamp.Format("2006.01.02"))

		spans, chains, err := trace.GetChains(rootSpanID)
		if err != nil {
			log.Errorf("Can not get chains of trace %s: %s", trace.ID, err)
			continue
		}

		doc.Fields["chains"] = chains
		docSpans := make([]map[string]interface{}, 0, len(spans))

		for _, span := range spans {
			ds := make(map[string]interface{})
			ds["spanid"] = span.ID
			if len(span.ParentSpanID) > 0 {
				ds["parentspanid"] = span.ParentSpanID
			}
			ds["prefix"] = span.Prefix
			for key, value := range span.Annotations {
				ds[key] = value
			}
			ds["cd"], ds["sd"], ds["td"] = span.Durations()

			if len(span.Timeline) > 0 {
				timeline := make(map[string]string)
				for key, timestamp := range span.Timeline {
					timeline[key] = timestamp.Value.Format(time.RFC3339Nano)
				}
				ds["timeline"] = timeline
			}
			docSpans = append(docSpans, ds)
		}
		doc.Fields["spans"] = docSpans
		documents = append(documents, doc)
	}
	return documents
}

// Collect completed traces and cleanout uncompleted
func (traceMap TraceMap) Collect(minTTL, maxTTL time.Duration, maxSpansPerTrace int, toES chan *Document) TraceMap {
	now := time.Now()

	defer metrics.FlushTimer.Update(time.Since(now))
	defer atomic.AddInt64(&metrics.TracesPending, int64(len(traceMap)))

	var (
		completed              int64
		uncompleted            int64
		nextGenerationTraceMap = make(TraceMap)
	)
	for traceID, trace := range traceMap {
		spansCount := len(trace.Spans)
		if spansCount > maxSpansPerTrace {
			log.Warningf("Trace %s spans limit %d overflow", traceID, maxSpansPerTrace)
		}
		if trace.Timestamp.Add(minTTL).After(now) && spansCount <= maxSpansPerTrace {
			nextGenerationTraceMap[traceID] = trace
			continue
		}
		if trace.Completed {
			completed++
			documents := trace.GetESDocuments()
			for _, doc := range documents {
				toES <- doc
			}
		} else {
			if trace.Timestamp.Add(maxTTL).After(now) && spansCount <= maxSpansPerTrace {
				nextGenerationTraceMap[traceID] = trace
				continue
			} else {
				uncompleted++
			}
		}
	}

	atomic.AddInt64(&metrics.TracesCompleted, completed)
	atomic.AddInt64(&metrics.TracesUncompleted, uncompleted)
	return nextGenerationTraceMap
}

func elasticSender(ch chan *Document, urls []string, bulkSize int, interval time.Duration) {
	log.Infof("Connecting to ES: %s", urls)
	// esClient, err := elastic.NewClient(elastic.SetURL(urls...))
	esClient, err := ESClientFactory(urls)
	if err != nil {
		log.Errorf("Can not connect to elasticsearch: %s", err)
	}

	bulk := make([]*Document, 0, bulkSize)
	timer := time.NewTimer(interval)
	var (
		ok  = true
		doc *Document
		wg  sync.WaitGroup
	)
	defer wg.Wait()
For:
	for {
		select {
		case doc, ok = <-ch:
			if !ok {
				break
			}
			bulk = append(bulk, doc)
			if len(bulk) >= bulkSize {
				break
			}
			continue For
		case <-timer.C:
			timer = time.NewTimer(interval)
		}

		wg.Add(1)
		go func(b []*Document) {
			defer wg.Done()
			sendBulk(esClient, b)
		}(bulk)
		if !ok {
			return
		}
		bulk = make([]*Document, 0, bulkSize)
	}
}

func sendBulk(esClient ESClient, documents []*Document) {
	defer atomic.AddInt64(&metrics.ActiveESRequests, -1)
	atomic.AddInt64(&metrics.ActiveESRequests, 1)
	if len(documents) == 0 {
		return
	}

	bulkRequest := esClient.Bulk()
	for _, m := range documents {
		bulkRequest = bulkRequest.Add(esClient.NewBulkIndexRequest().Index(m.Index).Type(esDocType).Doc(m.Fields))
	}
	res, err := bulkRequest.Do()
	if err != nil {
		log.Warningf("Send bulk failed: %s", err.Error())
		atomic.AddInt64(&metrics.FailedESRequests, 1)
		return
	}

	failedTraces := res.Failed()
	atomic.AddInt64(&metrics.FailedESTraces, int64(len(failedTraces)))
	log.Debugf("Indexed %d, failed %d of %d by %d ms", len(res.Indexed()), len(failedTraces), len(documents), res.Took)
	for i, res := range failedTraces {
		if i > 5 {
			log.Debug("Others response error details are omitted")
			break
		}
		response, err := json.Marshal(res.Error)
		if err != nil {
			log.Warningf("Can not decode response error: %s", err)
			continue
		}
		log.Warningf("Fail details [#%d] index [%s] %s", i, res.Index, string(response))
	}
}

type realESClient struct {
	client *elastic.Client
}

type realESBulkService struct {
	bulk *elastic.BulkService
}

func (b *realESBulkService) Add(r ESBulkRequest) ESBulkService {
	b.bulk = b.bulk.Add(r.(*realESBulkRequest).request)
	return b
}

func (b *realESBulkService) Do() (*elastic.BulkResponse, error) {
	return b.bulk.Do()
}

type realESBulkRequest struct {
	request *elastic.BulkIndexRequest
}

func (r *realESBulkRequest) Index(name string) ESBulkRequest {
	r.request = r.request.Index(name)
	return r
}

func (r *realESBulkRequest) Type(name string) ESBulkRequest {
	r.request = r.request.Type(name)
	return r
}

func (r *realESBulkRequest) Doc(doc interface{}) ESBulkRequest {
	r.request = r.request.Doc(doc)
	return r
}

func newESClient(urls []string) (ESClient, error) {
	esClient, err := elastic.NewClient(elastic.SetURL(urls...))
	if err != nil {
		return nil, err
	}
	return &realESClient{
		client: esClient,
	}, nil
}

func (realESClient *realESClient) NewBulkIndexRequest() ESBulkRequest {
	return &realESBulkRequest{
		request: elastic.NewBulkIndexRequest(),
	}
}

func (realESClient *realESClient) Bulk() ESBulkService {
	return &realESBulkService{
		bulk: realESClient.client.Bulk(),
	}
}
