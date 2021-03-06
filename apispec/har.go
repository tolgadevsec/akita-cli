package apispec

import (
	"github.com/google/martian/v3/har"
	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/akitasoftware/akita-cli/printer"
	hl "github.com/akitasoftware/akita-cli/har_loader"
	"github.com/akitasoftware/akita-cli/trace"
	"github.com/akitasoftware/akita-libs/akinet"
	"github.com/akitasoftware/akita-libs/sampled_err"
)

// Extract witnesses from a local HAR file and send them to the collector.
func processHAR(inboundCol, outboundCol trace.Collector, p string) error {
	harContent, err := hl.LoadCustomHARFromFile(p)
	if err != nil {
		return errors.Wrapf(err, "failed to load HAR file %s", p)
	}

	col := inboundCol
	if harContent.AkitaExt.Outbound {
		col = outboundCol
	}

	successCount, errs := parseFromHAR(col, harContent.Log)
	if errs.TotalCount > 0 {
		entriesCount := len(harContent.Log.Entries)
		printer.Stderr.Warningf("Encountered errors with %d HAR file entries.\n", entriesCount-successCount)
		printer.Stderr.Warningf("Akita will ignore entries with errors and generate a spec from the %d entries successfully processed.\n", successCount)

		printer.Stderr.Warningf("Sample errors:\n")
		for _, e := range errs.Samples {
			printer.Stderr.Warningf("\t- %s\n", e)
		}
	}

	return nil
}

func parseFromHAR(col trace.Collector, log *hl.CustomHARLog) (int, sampled_err.Errors) {
	// Use the same UUID for all witnesses from the same HAR file.
	harUUID := uuid.New()

	successfulEntries := 0
	errs := sampled_err.Errors{SampleCount: 3}
	for i, entry := range log.Entries {
		entrySuccess := true

		if entry.Request != nil {
			if req, err := convertRequest(entry.Request); err == nil {
				req.StreamID = harUUID
				req.Seq = i
				col.Process(akinet.ParsedNetworkTraffic{ObservationTime: entry.StartedDateTime, Content: req})
			} else {
				printer.Debugf("%s\n", errors.Wrapf(err, "failed to convert HAR request, index=%d", i))
				entrySuccess = false
				errs.Add(err)
			}
		}

		if entry.Response != nil {
			if resp, err := convertResponse(entry.Response); err == nil {
				resp.StreamID = harUUID
				resp.Seq = i
				col.Process(akinet.ParsedNetworkTraffic{ObservationTime: entry.StartedDateTime, Content: resp})
			} else {
				printer.Debugf("%s\n", errors.Wrapf(err, "failed to convert HAR response, index=%d", i))
				entrySuccess = false
				errs.Add(err)
			}
		}

		if entrySuccess {
			successfulEntries += 1
		}
	}
	return successfulEntries, errs
}

func convertRequest(harReq *har.Request) (akinet.HTTPRequest, error) {
	var req akinet.HTTPRequest
	err := req.FromHAR(harReq)
	return req, err
}

func convertResponse(harResp *har.Response) (akinet.HTTPResponse, error) {
	var resp akinet.HTTPResponse
	err := resp.FromHAR(harResp)
	return resp, err
}
