package trace

import (
	"context"
	"encoding/base64"
	"net"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"

	"github.com/akitasoftware/akita-cli/learn"
	"github.com/akitasoftware/akita-cli/printer"
	"github.com/akitasoftware/akita-cli/rest"
	"github.com/akitasoftware/akita-libs/akid"
	"github.com/akitasoftware/akita-libs/akinet"
	"github.com/akitasoftware/akita-libs/batcher"
	"github.com/akitasoftware/akita-libs/pbhash"
	pb "github.com/akitasoftware/akita-ir/go/api_spec"
	"github.com/akitasoftware/akita-cli/plugin"
	kgxapi "github.com/akitasoftware/akita-libs/api_schema"
)

const (
	// We stop trying to pair partial witnesses older than pairCacheExpiration.
	pairCacheExpiration = time.Minute

	// How often we clean out stale partial witnesses from pairCache.
	pairCacheCleanupInterval = 30 * time.Second

	// Max size per upload batch.
	uploadBatchMaxSize = 10

	// How often to flush the upload batch.
	uploadBatchFlushDuration = 30 * time.Second
)

type witnessWithInfo struct {
	srcIP           net.IP
	srcPort         uint16
	dstIP           net.IP
	dstPort         uint16
	observationTime time.Time
	id              akid.WitnessID

	witness *pb.Witness
}

func (r witnessWithInfo) toReport(dir kgxapi.NetworkDirection) (*kgxapi.WitnessReport, error) {
	// Hash algorithm defined in
	// https://docs.google.com/document/d/1ZANeoLTnsO10DcuzsAt6PBCt2MWLYW8oeu_A6d9bTJk/edit#heading=h.tbvm9waph6eu
	hash, err := pbhash.HashProto(r.witness)
	if err != nil {
		return nil, errors.Wrap(err, "failed to hash witness proto")
	}

	b, err := proto.Marshal(r.witness)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal witness proto")
	}

	return &kgxapi.WitnessReport{
		Direction:       dir,
		OriginAddr:      r.srcIP,
		OriginPort:      r.srcPort,
		DestinationAddr: r.dstIP,
		DestinationPort: r.dstPort,

		WitnessProto:      base64.URLEncoding.EncodeToString(b),
		ClientWitnessTime: r.observationTime,
		Hash:              hash,
		ID:                r.id,
	}, nil
}

// Sends witnesses up to akita cloud.
type BackendCollector struct {
	serviceID      akid.ServiceID
	learnSessionID akid.LearnSessionID
	learnClient    rest.LearnClient
	dir            kgxapi.NetworkDirection

	// Cache un-paired partial witnesses by pair key.
	pairCache map[akid.WitnessID]*witnessWithInfo

	// Batch of witnesses pending upload.
	uploadBatch *batcher.InMemory

	// Time when last process call was made.
	lastProcessTime time.Time

	plugins []plugin.AkitaPlugin
}

func NewBackendCollector(svc akid.ServiceID,
	lrn akid.LearnSessionID, lc rest.LearnClient, dir kgxapi.NetworkDirection,
	plugins []plugin.AkitaPlugin) Collector {
	col := &BackendCollector{
		serviceID:      svc,
		learnSessionID: lrn,
		learnClient:    lc,
		dir:            dir,
		pairCache:      map[akid.WitnessID]*witnessWithInfo{},
		plugins:        plugins,
	}

	col.uploadBatch = batcher.NewInMemory(
		col.uploadWitnesses,
		uploadBatchMaxSize,
		uploadBatchFlushDuration)
	return col
}

func (c *BackendCollector) Process(t akinet.ParsedNetworkTraffic) error {
	defer func() {
		// Lazily flush pair cache.
		now := time.Now()
		if !c.lastProcessTime.IsZero() && now.Sub(c.lastProcessTime) > pairCacheCleanupInterval {
			c.flushPairCache(now.Add(-1 * pairCacheExpiration))
		}
		c.lastProcessTime = now
	}()

	var isRequest bool
	var partial *learn.PartialWitness
	var parseHTTPErr error
	switch c := t.Content.(type) {
	case akinet.HTTPRequest:
		isRequest = true
		partial, parseHTTPErr = learn.ParseHTTP(c)
	case akinet.HTTPResponse:
		partial, parseHTTPErr = learn.ParseHTTP(c)
	default:
		// Non-HTTP traffic not handled
		return nil
	}

	if parseHTTPErr != nil {
		printer.Debugf("Failed to parse HTTP, skipping: %v\n", parseHTTPErr)
		return nil
	}

	if pair, ok := c.pairCache[partial.PairKey]; ok {
		// Combine the pair, merging the result into the existing item
		// rather than the new partial.
		learn.MergeWitness(pair.witness, partial.Witness)
		delete(c.pairCache, partial.PairKey)

		// If partial is the request, flip the src/dst in the pair before
		// reporting.
		if isRequest {
			pair.srcIP, pair.dstIP = pair.dstIP, pair.srcIP
			pair.srcPort, pair.dstPort = pair.dstPort, pair.srcPort
		}

		c.queueUpload(pair)
	} else {
		// Store the partial witness for now, waiting for its pair or a
		// flush timeout.
		c.pairCache[partial.PairKey] = &witnessWithInfo{
			srcIP:           t.SrcIP,
			srcPort:         uint16(t.SrcPort),
			dstIP:           t.DstIP,
			dstPort:         uint16(t.DstPort),
			witness:         partial.Witness,
			observationTime: t.ObservationTime,
			id:              partial.PairKey,
		}
	}
	return nil
}

func (c *BackendCollector) queueUpload(w *witnessWithInfo) {
	for _, p := range c.plugins {
		if err := p.Transform(w.witness.GetMethod()); err != nil {
			// Only upload if plugins did not return error.
			printer.Errorf("plugin %q returned error, skipping: %v", p.Name(), err)
			return
		}
	}

	// Obfuscate the original value so type inference engine can use it on the
	// backend without revealing the actual value.
	obfuscate(w.witness.GetMethod())
	c.uploadBatch.Add(w)
}

func (c *BackendCollector) Close() error {
	c.flushPairCache(time.Now())
	c.uploadBatch.Close()
	return nil
}

func (c *BackendCollector) uploadWitnesses(in []interface{}) {
	reports := make([]*kgxapi.WitnessReport, 0, len(in))
	for _, i := range in {
		w := i.(*witnessWithInfo)
		r, err := w.toReport(c.dir)
		if err == nil {
			reports = append(reports, r)
		} else {
			printer.Warningf("Failed to convert witness to report: %v\n", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.learnClient.ReportWitnesses(ctx, c.learnSessionID, reports)
	if err != nil {
		printer.Warningf("Failed to upload witnesses: %v\n", err)
	}
}

func (c *BackendCollector) flushPairCache(cutoffTime time.Time) {
	for k, e := range c.pairCache {
		if e.observationTime.Before(cutoffTime) {
			c.queueUpload(e)
			delete(c.pairCache, k)
		}
	}
}
