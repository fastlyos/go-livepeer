package monitor

import (
	"context"
	"math/big"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	"contrib.go.opencensus.io/exporter/prometheus"
	rprom "github.com/prometheus/client_golang/prometheus"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
)

type (
	SegmentUploadError    string
	SegmentTranscodeError string
)

const (
	SegmentUploadErrorUnknown               SegmentUploadError    = "Unknown"
	SegmentUploadErrorGenCreds              SegmentUploadError    = "GenCreds"
	SegmentUploadErrorOS                    SegmentUploadError    = "ObjectStorage"
	SegmentUploadErrorSessionEnded          SegmentUploadError    = "SessionEnded"
	SegmentUploadErrorInsufficientBalance   SegmentUploadError    = "InsufficientBalance"
	SegmentUploadErrorTimeout               SegmentUploadError    = "Timeout"
	SegmentTranscodeErrorUnknown            SegmentTranscodeError = "Unknown"
	SegmentTranscodeErrorUnknownResponse    SegmentTranscodeError = "UnknownResponse"
	SegmentTranscodeErrorTranscode          SegmentTranscodeError = "Transcode"
	SegmentTranscodeErrorOrchestratorBusy   SegmentTranscodeError = "OrchestratorBusy"
	SegmentTranscodeErrorOrchestratorCapped SegmentTranscodeError = "OrchestratorCapped"
	SegmentTranscodeErrorParseResponse      SegmentTranscodeError = "ParseResponse"
	SegmentTranscodeErrorReadBody           SegmentTranscodeError = "ReadBody"
	SegmentTranscodeErrorNoOrchestrators    SegmentTranscodeError = "NoOrchestrators"
	SegmentTranscodeErrorDownload           SegmentTranscodeError = "Download"
	SegmentTranscodeErrorSaveData           SegmentTranscodeError = "SaveData"
	SegmentTranscodeErrorSessionEnded       SegmentTranscodeError = "SessionEnded"
	SegmentTranscodeErrorPlaylist           SegmentTranscodeError = "Playlist"

	numberOfSegmentsToCalcAverage = 30
	gweiConversionFactor          = 1000000000

	logLevel = 6 // TODO move log levels definitions to separate package
	// importing `common` package here introduces import cycles
)

// Enabled true if metrics was enabled in command line
var Enabled bool

var timeToWaitForError = 8500 * time.Millisecond
var timeoutWatcherPause = 15 * time.Second

type (
	censusMetricsCounter struct {
		nodeType                                 string
		nodeID                                   string
		ctx                                      context.Context
		kGPU                                     tag.Key
		kNodeType                                tag.Key
		kNodeID                                  tag.Key
		kProfile                                 tag.Key
		kProfiles                                tag.Key
		kErrorCode                               tag.Key
		kTry                                     tag.Key
		kSender                                  tag.Key
		kRecipient                               tag.Key
		kManifestID                              tag.Key
		mSegmentSourceAppeared                   *stats.Int64Measure
		mSegmentEmerged                          *stats.Int64Measure
		mSegmentEmergedUnprocessed               *stats.Int64Measure
		mSegmentUploaded                         *stats.Int64Measure
		mSegmentUploadFailed                     *stats.Int64Measure
		mSegmentTranscoded                       *stats.Int64Measure
		mSegmentTranscodedUnprocessed            *stats.Int64Measure
		mSegmentTranscodeFailed                  *stats.Int64Measure
		mSegmentTranscodedAppeared               *stats.Int64Measure
		mSegmentTranscodedAllAppeared            *stats.Int64Measure
		mStartBroadcastClientFailed              *stats.Int64Measure
		mStreamCreateFailed                      *stats.Int64Measure
		mStreamCreated                           *stats.Int64Measure
		mStreamStarted                           *stats.Int64Measure
		mStreamEnded                             *stats.Int64Measure
		mMaxSessions                             *stats.Int64Measure
		mCurrentSessions                         *stats.Int64Measure
		mDiscoveryError                          *stats.Int64Measure
		mTranscodeRetried                        *stats.Int64Measure
		mTranscodersNumber                       *stats.Int64Measure
		mTranscodersCapacity                     *stats.Int64Measure
		mTranscodersLoad                         *stats.Int64Measure
		mSuccessRate                             *stats.Float64Measure
		mTranscodeTime                           *stats.Float64Measure
		mTranscodeTimeAvg                        *stats.Float64Measure
		mTranscodeTimeRealtimeRatio              *stats.Float64Measure
		mTranscodeTimeRealtimeRatioAvg           *stats.Float64Measure
		mTranscodeLatency                        *stats.Float64Measure
		mTranscodeOverallLatency                 *stats.Float64Measure
		mTranscodeOverallLatencyAvg              *stats.Float64Measure
		mTranscodeOverallLatencyRealtimeRatio    *stats.Float64Measure
		mTranscodeOverallLatencyRealtimeRatioAvg *stats.Float64Measure
		mUploadTime                              *stats.Float64Measure
		mAuthWebhookTime                         *stats.Float64Measure
		mSourceSegmentDuration                   *stats.Float64Measure

		// Metrics for sending payments
		mTicketValueSent    *stats.Float64Measure
		mTicketsSent        *stats.Int64Measure
		mPaymentCreateError *stats.Int64Measure
		mDeposit            *stats.Float64Measure
		mReserve            *stats.Float64Measure

		// Metrics for receiving payments
		mTicketValueRecv       *stats.Float64Measure
		mTicketsRecv           *stats.Int64Measure
		mPaymentRecvErr        *stats.Int64Measure
		mWinningTicketsRecv    *stats.Int64Measure
		mValueRedeemed         *stats.Float64Measure
		mTicketRedemptionError *stats.Int64Measure
		mSuggestedGasPrice     *stats.Float64Measure
		mTranscodingPrice      *stats.Float64Measure

		lock                                sync.Mutex
		emergeTimes                         map[uint64]map[uint64]time.Time // nonce:seqNo
		success                             map[uint64]*segmentsAverager
		transcodeOverallLatencyAverage      *movingAverage
		transcodeOverallLatencyRatioAverage *movingAverage
		transcodeTimeAverage                *movingAverage
		transcodeTimeRatioAverage           *movingAverage
	}

	segmentCount struct {
		seqNo       uint64
		emergedTime time.Time
		emerged     int
		transcoded  int
		failed      bool
	}

	tryData struct {
		first time.Time
		tries int
	}

	segmentsAverager struct {
		segments  []segmentCount
		start     int
		end       int
		removed   bool
		removedAt time.Time
		tries     map[uint64]tryData // seqNo:try
	}

	movingAverage struct {
		windowSize time.Duration
		data       *ringArray
	}
	timeValue struct {
		time  time.Time
		value float64
	}

	ringArray struct {
		data []timeValue
		head int
		tail int
	}
)

// Exporter Prometheus exporter that handles `/metrics` endpoint
var Exporter *prometheus.Exporter

var census censusMetricsCounter

// used in unit tests
var unitTestMode bool

func InitCensus(nodeType, nodeID, version string) {
	census = censusMetricsCounter{
		emergeTimes:                         make(map[uint64]map[uint64]time.Time),
		nodeID:                              nodeID,
		nodeType:                            nodeType,
		success:                             make(map[uint64]*segmentsAverager),
		transcodeOverallLatencyAverage:      newMovingAverage(time.Minute, 128),
		transcodeOverallLatencyRatioAverage: newMovingAverage(time.Minute, 128),
		transcodeTimeAverage:                newMovingAverage(time.Minute, 128),
		transcodeTimeRatioAverage:           newMovingAverage(time.Minute, 128),
	}
	var err error
	ctx := context.Background()
	census.kGPU = tag.MustNewKey("gpu")
	census.kNodeType = tag.MustNewKey("node_type")
	census.kNodeID = tag.MustNewKey("node_id")
	census.kProfile = tag.MustNewKey("profile")
	census.kProfiles = tag.MustNewKey("profiles")
	census.kErrorCode = tag.MustNewKey("error_code")
	census.kTry = tag.MustNewKey("try")
	census.kSender = tag.MustNewKey("sender")
	census.kRecipient = tag.MustNewKey("recipient")
	census.kManifestID = tag.MustNewKey("manifestID")
	census.ctx, err = tag.New(ctx, tag.Insert(census.kNodeType, nodeType), tag.Insert(census.kNodeID, nodeID))
	if err != nil {
		glog.Fatal("Error creating context", err)
	}
	census.mSegmentSourceAppeared = stats.Int64("segment_source_appeared_total", "SegmentSourceAppeared", "tot")
	census.mSegmentEmerged = stats.Int64("segment_source_emerged_total", "SegmentEmerged", "tot")
	census.mSegmentEmergedUnprocessed = stats.Int64("segment_source_emerged_unprocessed_total", "SegmentEmerged, counted by number of transcode profiles", "tot")
	census.mSegmentUploaded = stats.Int64("segment_source_uploaded_total", "SegmentUploaded", "tot")
	census.mSegmentUploadFailed = stats.Int64("segment_source_upload_failed_total", "SegmentUploadedFailed", "tot")
	census.mSegmentTranscoded = stats.Int64("segment_transcoded_total", "SegmentTranscoded", "tot")
	census.mSegmentTranscodedUnprocessed = stats.Int64("segment_transcoded_unprocessed_total", "SegmentTranscodedUnprocessed", "tot")
	census.mSegmentTranscodeFailed = stats.Int64("segment_transcode_failed_total", "SegmentTranscodeFailed", "tot")
	census.mSegmentTranscodedAppeared = stats.Int64("segment_transcoded_appeared_total", "SegmentTranscodedAppeared", "tot")
	census.mSegmentTranscodedAllAppeared = stats.Int64("segment_transcoded_all_appeared_total", "SegmentTranscodedAllAppeared", "tot")
	census.mStartBroadcastClientFailed = stats.Int64("broadcast_client_start_failed_total", "StartBroadcastClientFailed", "tot")
	census.mStreamCreateFailed = stats.Int64("stream_create_failed_total", "StreamCreateFailed", "tot")
	census.mStreamCreated = stats.Int64("stream_created_total", "StreamCreated", "tot")
	census.mStreamStarted = stats.Int64("stream_started_total", "StreamStarted", "tot")
	census.mStreamEnded = stats.Int64("stream_ended_total", "StreamEnded", "tot")
	census.mMaxSessions = stats.Int64("max_sessions_total", "MaxSessions", "tot")
	census.mCurrentSessions = stats.Int64("current_sessions_total", "Number of currently transcded streams", "tot")
	census.mDiscoveryError = stats.Int64("discovery_errors_total", "Number of discover errors", "tot")
	census.mTranscodeRetried = stats.Int64("transcode_retried", "Number of times segment transcode was retried", "tot")
	census.mTranscodersNumber = stats.Int64("transcoders_number", "Number of transcoders currently connected to orchestrator", "tot")
	census.mTranscodersCapacity = stats.Int64("transcoders_capacity", "Total advertised capacity of transcoders currently connected to orchestrator", "tot")
	census.mTranscodersLoad = stats.Int64("transcoders_load", "Total load of transcoders currently connected to orchestrator", "tot")
	census.mSuccessRate = stats.Float64("success_rate", "Success rate", "per")
	census.mTranscodeTime = stats.Float64("transcode_time_seconds", "Transcoding time", "sec")
	census.mTranscodeTimeAvg = stats.Float64("transcode_time_avg_seconds", "Transcoding time average over one minute", "sec")
	census.mTranscodeTimeRealtimeRatio = stats.Float64("transcode_time_realtime_ratio", "Transcoding time divided by segment's duration", "perc")
	census.mTranscodeTimeRealtimeRatioAvg = stats.Float64("transcode_time_realtime_ratio_avg", "Transcoding time divided by segment's duration average over one minute", "perc")
	census.mTranscodeLatency = stats.Float64("transcode_latency_seconds",
		"Transcoding latency, from source segment emerged from segmenter till transcoded segment appeared in manifest", "sec")
	census.mTranscodeOverallLatency = stats.Float64("transcode_overall_latency_seconds",
		"Transcoding latency, from source segment emerged from segmenter till all transcoded segment appeared in manifest", "sec")
	census.mTranscodeOverallLatencyAvg = stats.Float64("transcode_overall_latency_avg_seconds",
		"Transcoding latency average over one minute, from source segment emerged from segmenter till all transcoded segment apeeared in manifest", "sec")
	census.mTranscodeOverallLatencyRealtimeRatio = stats.Float64("transcode_overall_latency_realtime_ratio",
		"Transcoding latency divided by segment's duration", "perc")
	census.mTranscodeOverallLatencyRealtimeRatioAvg = stats.Float64("transcode_overall_latency_realtime_ratio_avg",
		"Transcoding latency divided by segment's duration average over one minute", "perc")
	census.mUploadTime = stats.Float64("upload_time_seconds", "Upload (to Orchestrator) time", "sec")
	census.mAuthWebhookTime = stats.Float64("auth_webhook_time_milliseconds", "Authentication webhook execution time", "ms")
	census.mSourceSegmentDuration = stats.Float64("source_segment_duration_seconds", "Source segment's duration", "sec")

	// Metrics for sending payments
	census.mTicketValueSent = stats.Float64("ticket_value_sent", "TicketValueSent", "gwei")
	census.mTicketsSent = stats.Int64("tickets_sent", "TicketsSent", "tot")
	census.mPaymentCreateError = stats.Int64("payment_create_errors", "PaymentCreateError", "tot")
	census.mDeposit = stats.Float64("broadcaster_deposit", "Current remaining deposit for the broadcaster node", "gwei")
	census.mReserve = stats.Float64("broadcaster_reserve", "Current remaiing reserve for the broadcaster node", "gwei")

	// Metrics for receiving payments
	census.mTicketValueRecv = stats.Float64("ticket_value_recv", "TicketValueRecv", "gwei")
	census.mTicketsRecv = stats.Int64("tickets_recv", "TicketsRecv", "tot")
	census.mPaymentRecvErr = stats.Int64("payment_recv_errors", "PaymentRecvErr", "tot")
	census.mWinningTicketsRecv = stats.Int64("winning_tickets_recv", "WinningTicketsRecv", "tot")
	census.mValueRedeemed = stats.Float64("value_redeemed", "ValueRedeemed", "gwei")
	census.mTicketRedemptionError = stats.Int64("ticket_redemption_errors", "TicketRedemptionError", "tot")
	census.mSuggestedGasPrice = stats.Float64("suggested_gas_price", "SuggestedGasPrice", "gwei")
	census.mTranscodingPrice = stats.Float64("transcoding_price", "TranscodingPrice", "wei")

	glog.Infof("Compiler: %s Arch %s OS %s Go version %s", runtime.Compiler, runtime.GOARCH, runtime.GOOS, runtime.Version())
	glog.Infof("Livepeer version: %s", version)
	glog.Infof("Node type %s node ID %s", nodeType, nodeID)
	mVersions := stats.Int64("versions", "Version information.", "Num")
	compiler := tag.MustNewKey("compiler")
	goarch := tag.MustNewKey("goarch")
	goos := tag.MustNewKey("goos")
	goversion := tag.MustNewKey("goversion")
	livepeerversion := tag.MustNewKey("livepeerversion")
	ctx, err = tag.New(ctx, tag.Insert(census.kNodeType, nodeType), tag.Insert(census.kNodeID, nodeID),
		tag.Insert(compiler, runtime.Compiler), tag.Insert(goarch, runtime.GOARCH), tag.Insert(goos, runtime.GOOS),
		tag.Insert(goversion, runtime.Version()), tag.Insert(livepeerversion, version))
	if err != nil {
		glog.Fatal("Error creating tagged context", err)
	}
	baseTags := []tag.Key{census.kNodeID, census.kNodeType}
	views := []*view.View{
		{
			Name:        "versions",
			Measure:     mVersions,
			Description: "Versions used by LivePeer node.",
			TagKeys:     []tag.Key{census.kNodeType, compiler, goos, goversion, livepeerversion},
			Aggregation: view.LastValue(),
		},
		{
			Name:        "broadcast_client_start_failed_total",
			Measure:     census.mStartBroadcastClientFailed,
			Description: "StartBroadcastClientFailed",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "stream_created_total",
			Measure:     census.mStreamCreated,
			Description: "StreamCreated",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "stream_started_total",
			Measure:     census.mStreamStarted,
			Description: "StreamStarted",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "stream_ended_total",
			Measure:     census.mStreamEnded,
			Description: "StreamEnded",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "stream_create_failed_total",
			Measure:     census.mStreamCreateFailed,
			Description: "StreamCreateFailed",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_source_appeared_total",
			Measure:     census.mSegmentSourceAppeared,
			Description: "SegmentSourceAppeared",
			TagKeys:     append([]tag.Key{census.kProfile}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_source_emerged_total",
			Measure:     census.mSegmentEmerged,
			Description: "SegmentEmerged",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_source_emerged_unprocessed_total",
			Measure:     census.mSegmentEmergedUnprocessed,
			Description: "Raw number of segments emerged from segmenter.",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_source_uploaded_total",
			Measure:     census.mSegmentUploaded,
			Description: "SegmentUploaded",
			TagKeys:     baseTags,
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_source_upload_failed_total",
			Measure:     census.mSegmentUploadFailed,
			Description: "SegmentUploadedFailed",
			TagKeys:     append([]tag.Key{census.kErrorCode}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_transcoded_total",
			Measure:     census.mSegmentTranscoded,
			Description: "SegmentTranscoded",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_transcoded_unprocessed_total",
			Measure:     census.mSegmentTranscodedUnprocessed,
			Description: "Raw number of segments successfully transcoded.",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_transcode_failed_total",
			Measure:     census.mSegmentTranscodeFailed,
			Description: "SegmentTranscodeFailed",
			TagKeys:     append([]tag.Key{census.kErrorCode}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_transcoded_appeared_total",
			Measure:     census.mSegmentTranscodedAppeared,
			Description: "SegmentTranscodedAppeared",
			TagKeys:     append([]tag.Key{census.kProfile}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "segment_transcoded_all_appeared_total",
			Measure:     census.mSegmentTranscodedAllAppeared,
			Description: "SegmentTranscodedAllAppeared",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "success_rate",
			Measure:     census.mSuccessRate,
			Description: "Number of transcoded segments divided on number of source segments",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcode_time_seconds",
			Measure:     census.mTranscodeTime,
			Description: "TranscodeTime, seconds",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Distribution(0, .250, .500, .750, 1.000, 1.250, 1.500, 2.000, 2.500, 3.000, 3.500, 4.000, 4.500, 5.000, 10.000, 20.0, 30.0),
		},
		{
			Name:        "transcode_time_avg_seconds",
			Measure:     census.mTranscodeTimeAvg,
			Description: "TranscodeTime, seconds",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcode_time_realtime_ratio",
			Measure:     census.mTranscodeTimeRealtimeRatio,
			Description: "Transcoding time divided by segment duration",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Distribution(0, .050, .100, 0.250, .500, 0.750, 1.000, 1.250, 1.500, 2.0, 3.000, 4.000, 5.000, 10.0, 20.0, 50.0),
		},
		{
			Name:        "transcode_time_realtime_ratio_avg",
			Measure:     census.mTranscodeTimeRealtimeRatioAvg,
			Description: "Transcoding time divided by segment duration average over one minute",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcode_latency_seconds",
			Measure:     census.mTranscodeLatency,
			Description: "Transcoding latency, from source segment emerged from segmenter till transcoded segment apeeared in manifest",
			TagKeys:     append([]tag.Key{census.kProfile}, baseTags...),
			Aggregation: view.Distribution(0, .500, .75, 1.000, 1.500, 2.000, 2.500, 3.000, 3.500, 4.000, 4.500, 5.000, 10.000, 20.0, 30.0),
		},
		{
			Name:        "transcode_overall_latency_seconds",
			Measure:     census.mTranscodeOverallLatency,
			Description: "Transcoding latency, from source segment emerged from segmenter till all transcoded segment apeeared in manifest",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Distribution(0, .500, .75, 1.000, 1.500, 2.000, 2.500, 3.000, 3.500, 4.000, 4.500, 5.000, 10.000, 20.0, 30.0),
		},
		{
			Name:        "transcode_overall_latency_avg_seconds",
			Measure:     census.mTranscodeOverallLatencyAvg,
			Description: "Transcoding latency average over one minute, from source segment emerged from segmenter till all transcoded segment apeeared in manifest",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcode_overall_latency_realtime_ratio",
			Measure:     census.mTranscodeOverallLatencyRealtimeRatio,
			Description: "Transcoding latency divided by segment's duration",
			TagKeys:     append([]tag.Key{census.kProfiles}, baseTags...),
			Aggregation: view.Distribution(0, .050, .100, 0.250, .500, 0.750, 1.000, 1.250, 1.500, 2.0, 3.000, 4.000, 5.000, 10.0, 20.0, 50.0),
		},
		{
			Name:        "transcode_overall_latency_realtime_ratio_avg",
			Measure:     census.mTranscodeOverallLatencyRealtimeRatioAvg,
			Description: "Transcoding latency divided by segment's duration average over one minute",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "upload_time_seconds",
			Measure:     census.mUploadTime,
			Description: "UploadTime, seconds",
			TagKeys:     baseTags,
			Aggregation: view.Distribution(0, .10, .20, .50, .100, .150, .200, .500, .1000, .5000, 10.000),
		},
		{
			Name:        "auth_webhook_time_milliseconds",
			Measure:     census.mAuthWebhookTime,
			Description: "Authentication webhook execution time, milliseconds",
			TagKeys:     baseTags,
			Aggregation: view.Distribution(0, 100, 250, 500, 750, 1000, 1500, 2000, 2500, 3000, 5000, 10000),
		},
		{
			Name:        "source_segment_duration_seconds",
			Measure:     census.mSourceSegmentDuration,
			Description: "Source segment's duration",
			TagKeys:     baseTags,
			Aggregation: view.Distribution(0, 100, 250, 500, 750, 1000, 1500, 2000, 2500, 3000, 5000, 10000, 20000, 30000),
		},
		{
			Name:        "max_sessions_total",
			Measure:     census.mMaxSessions,
			Description: "Max Sessions",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "current_sessions_total",
			Measure:     census.mCurrentSessions,
			Description: "Number of streams currently transcoding",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "discovery_errors_total",
			Measure:     census.mDiscoveryError,
			Description: "Number of discover errors",
			TagKeys:     append([]tag.Key{census.kErrorCode}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "transcode_retried",
			Measure:     census.mTranscodeRetried,
			Description: "Number of times segment transcode was retried",
			TagKeys:     append([]tag.Key{census.kTry}, baseTags...),
			Aggregation: view.Count(),
		},
		{
			Name:        "transcoders_number",
			Measure:     census.mTranscodersNumber,
			Description: "Number of transcoders currently connected to orchestrator",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcoders_capacity",
			Measure:     census.mTranscodersCapacity,
			Description: "Total advertised capacity of transcoders currently connected to orchestrator",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcoders_load",
			Measure:     census.mTranscodersLoad,
			Description: "Total load of transcoders currently connected to orchestrator",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},

		// Metrics for sending payments
		{
			Name:        "ticket_value_sent",
			Measure:     census.mTicketValueSent,
			Description: "Ticket value sent",
			TagKeys:     append([]tag.Key{census.kRecipient, census.kManifestID}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "tickets_sent",
			Measure:     census.mTicketsSent,
			Description: "Tickets sent",
			TagKeys:     append([]tag.Key{census.kRecipient, census.kManifestID}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "payment_create_errors",
			Measure:     census.mPaymentCreateError,
			Description: "Errors when creating payments",
			TagKeys:     append([]tag.Key{census.kRecipient, census.kManifestID}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "broadcaster_deposit",
			Measure:     census.mDeposit,
			Description: "Current remaining deposit for the broadcaster node",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "broadcaster_reserve",
			Measure:     census.mReserve,
			Description: "Current remaining reserve for the broadcaster node",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},

		// Metrics for receiving payments
		{
			Name:        "ticket_value_recv",
			Measure:     census.mTicketValueRecv,
			Description: "Ticket value received",
			TagKeys:     append([]tag.Key{census.kSender, census.kManifestID}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "tickets_recv",
			Measure:     census.mTicketsRecv,
			Description: "Tickets received",
			TagKeys:     append([]tag.Key{census.kSender, census.kManifestID}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "payment_recv_errors",
			Measure:     census.mPaymentRecvErr,
			Description: "Errors when receiving payments",
			TagKeys:     append([]tag.Key{census.kSender, census.kManifestID, census.kErrorCode}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "winning_tickets_recv",
			Measure:     census.mWinningTicketsRecv,
			Description: "Winning tickets received",
			TagKeys:     baseTags,
			Aggregation: view.Sum(),
		},
		{
			Name:        "value_redeemed",
			Measure:     census.mValueRedeemed,
			Description: "Winning ticket value redeemed",
			TagKeys:     baseTags,
			Aggregation: view.Sum(),
		},
		{
			Name:        "ticket_redemption_errors",
			Measure:     census.mTicketRedemptionError,
			Description: "Errors when redeeming tickets",
			TagKeys:     append([]tag.Key{census.kSender}, baseTags...),
			Aggregation: view.Sum(),
		},
		{
			Name:        "suggested_gas_price",
			Measure:     census.mSuggestedGasPrice,
			Description: "Suggested gas price for winning ticket redemption",
			TagKeys:     baseTags,
			Aggregation: view.LastValue(),
		},
		{
			Name:        "transcoding_price",
			Measure:     census.mTranscodingPrice,
			Description: "Transcoding price per pixel",
			TagKeys:     append([]tag.Key{census.kSender}, baseTags...),
			Aggregation: view.LastValue(),
		},
	}

	// Register the views
	if err := view.Register(views...); err != nil {
		glog.Fatalf("Failed to register views: %v", err)
	}
	registry := rprom.NewRegistry()
	registry.MustRegister(rprom.NewProcessCollector(rprom.ProcessCollectorOpts{}))
	registry.MustRegister(rprom.NewGoCollector())
	pe, err := prometheus.NewExporter(prometheus.Options{
		Namespace: "livepeer",
		Registry:  registry,
	})
	if err != nil {
		glog.Fatalf("Failed to create the Prometheus stats exporter: %v", err)
	}

	// Register the Prometheus exporters as a stats exporter.
	view.RegisterExporter(pe)
	stats.Record(ctx, mVersions.M(1))
	ctx, err = tag.New(census.ctx, tag.Insert(census.kErrorCode, "LostSegment"))
	if err != nil {
		glog.Fatal("Error creating context", err)
	}
	if !unitTestMode {
		go census.timeoutWatcher(ctx)
	}
	Exporter = pe

	// init metrics values
	SetTranscodersNumberAndLoad(0, 0, 0)
}

// LogDiscoveryError records discovery error
func LogDiscoveryError(code string) {
	glog.Error("Discovery error=" + code)
	if strings.Contains(code, "OrchestratorCapped") {
		code = "OrchestratorCapped"
	} else if strings.Contains(code, "Canceled") {
		code = "Canceled"
	}
	ctx, err := tag.New(census.ctx, tag.Insert(census.kErrorCode, code))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}
	stats.Record(ctx, census.mDiscoveryError.M(1))
}

func (cen *censusMetricsCounter) successRate() float64 {
	var i int
	var f float64
	if len(cen.success) == 0 {
		return 1
	}
	for _, avg := range cen.success {
		if r, has := avg.successRate(); has {
			i++
			f += r
		}
	}
	if i > 0 {
		return f / float64(i)
	}
	return 1
}

func (sa *segmentsAverager) successRate() (float64, bool) {
	var emerged, transcoded int
	if sa.end == -1 {
		return 1, false
	}
	i := sa.start
	now := time.Now()
	for {
		item := &sa.segments[i]
		if item.transcoded > 0 || item.failed || now.Sub(item.emergedTime) > timeToWaitForError {
			emerged += item.emerged
			transcoded += item.transcoded
		}
		if i == sa.end {
			break
		}
		i = sa.advance(i)
	}
	if emerged > 0 {
		return float64(transcoded) / float64(emerged), true
	}
	return 1, false
}

func (sa *segmentsAverager) advance(i int) int {
	i++
	if i == len(sa.segments) {
		i = 0
	}
	return i
}

func (sa *segmentsAverager) addEmerged(seqNo uint64) {
	item, _ := sa.getAddItem(seqNo)
	item.emerged = 1
	item.transcoded = 0
	item.emergedTime = time.Now()
	item.seqNo = seqNo
}

func (sa *segmentsAverager) addTranscoded(seqNo uint64, failed bool) {
	item, found := sa.getAddItem(seqNo)
	if !found {
		item.emerged = 0
		item.emergedTime = time.Now()
	}
	item.failed = failed
	if !failed {
		item.transcoded = 1
	}
	item.seqNo = seqNo
}

func (sa *segmentsAverager) getAddItem(seqNo uint64) (*segmentCount, bool) {
	var index int
	if sa.end == -1 {
		sa.end = 0
	} else {
		i := sa.start
		for {
			if sa.segments[i].seqNo == seqNo {
				return &sa.segments[i], true
			}
			if i == sa.end {
				break
			}
			i = sa.advance(i)
		}
		sa.end = sa.advance(sa.end)
		index = sa.end
		if sa.end == sa.start {
			sa.start = sa.advance(sa.start)
		}
	}
	return &sa.segments[index], false
}

func (sa *segmentsAverager) canBeRemoved() bool {
	if sa.end == -1 {
		return true
	}
	i := sa.start
	now := time.Now()
	for {
		item := &sa.segments[i]
		if item.transcoded == 0 && !item.failed && now.Sub(item.emergedTime) <= timeToWaitForError {
			return false
		}
		if i == sa.end {
			break
		}
		i = sa.advance(i)
	}
	return true
}

func (cen *censusMetricsCounter) timeoutWatcher(ctx context.Context) {
	for {
		cen.lock.Lock()
		now := time.Now()
		for nonce, emerged := range cen.emergeTimes {
			for seqNo, tm := range emerged {
				ago := now.Sub(tm)
				if ago > timeToWaitForError {
					stats.Record(cen.ctx, cen.mSegmentEmerged.M(1))
					delete(emerged, seqNo)
					// This shouldn't happen, but if it is, we record
					// `LostSegment` error, to try to find out why we missed segment
					stats.Record(ctx, cen.mSegmentTranscodeFailed.M(1))
					glog.Errorf("LostSegment nonce=%d seqNo=%d emerged=%ss ago", nonce, seqNo, ago)
				}
			}
		}
		cen.sendSuccess()
		for nonce, avg := range cen.success {
			if avg.removed && now.Sub(avg.removedAt) > 2*timeToWaitForError {
				// need to keep this around for some time to give Prometheus chance to scrape this value
				// (Prometheus scrapes every 5 seconds)
				delete(cen.success, nonce)
			} else {
				for seqNo, tr := range avg.tries {
					if now.Sub(tr.first) > 2*timeToWaitForError {
						delete(avg.tries, seqNo)
					}
				}
			}
		}
		cen.lock.Unlock()
		time.Sleep(timeoutWatcherPause)
	}
}

func MaxSessions(maxSessions int) {
	census.lock.Lock()
	defer census.lock.Unlock()
	stats.Record(census.ctx, census.mMaxSessions.M(int64(maxSessions)))
}

func CurrentSessions(currentSessions int) {
	census.lock.Lock()
	defer census.lock.Unlock()
	stats.Record(census.ctx, census.mCurrentSessions.M(int64(currentSessions)))
}

func TranscodeTry(nonce, seqNo uint64) {
	census.lock.Lock()
	defer census.lock.Unlock()
	if av, ok := census.success[nonce]; ok {
		if av.tries == nil {
			av.tries = make(map[uint64]tryData)
		}
		try := 1
		if ts, tok := av.tries[seqNo]; tok {
			ts.tries++
			try = ts.tries
			av.tries[seqNo] = ts
			label := ">10"
			if ts.tries < 11 {
				label = strconv.Itoa(ts.tries)
			}
			ctx, err := tag.New(census.ctx, tag.Insert(census.kTry, label))
			if err != nil {
				glog.Error("Error creating context", err)
				return
			}
			stats.Record(ctx, census.mTranscodeRetried.M(1))
		} else {
			av.tries[seqNo] = tryData{tries: 1, first: time.Now()}
		}
		glog.V(logLevel).Infof("Trying to transcode segment nonce=%d seqNo=%d try=%d", nonce, seqNo, try)
	}
}

func SetTranscodersNumberAndLoad(load, capacity, number int) {
	census.lock.Lock()
	defer census.lock.Unlock()
	stats.Record(census.ctx, census.mTranscodersLoad.M(int64(load)))
	stats.Record(census.ctx, census.mTranscodersCapacity.M(int64(capacity)))
	stats.Record(census.ctx, census.mTranscodersNumber.M(int64(number)))
}

func SegmentEmerged(nonce, seqNo uint64, profilesNum int, dur float64) {
	glog.V(logLevel).Infof("Logging SegmentEmerged... nonce=%d seqNo=%d duration=%v", nonce, seqNo, dur)
	census.segmentEmerged(nonce, seqNo, profilesNum)
	stats.Record(census.ctx, census.mSourceSegmentDuration.M(dur))
}

func (cen *censusMetricsCounter) segmentEmerged(nonce, seqNo uint64, profilesNum int) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	if _, has := cen.emergeTimes[nonce]; !has {
		cen.emergeTimes[nonce] = make(map[uint64]time.Time)
	}
	if avg, has := cen.success[nonce]; has {
		avg.addEmerged(seqNo)
	}
	cen.emergeTimes[nonce][seqNo] = time.Now()
	stats.Record(cen.ctx, cen.mSegmentEmergedUnprocessed.M(1))
}

func SourceSegmentAppeared(nonce, seqNo uint64, manifestID, profile string) {
	glog.V(logLevel).Infof("Logging SourceSegmentAppeared... nonce=%d manifestID=%s seqNo=%d profile=%s", nonce,
		manifestID, seqNo, profile)
	census.segmentSourceAppeared(nonce, seqNo, profile)
}

func (cen *censusMetricsCounter) segmentSourceAppeared(nonce, seqNo uint64, profile string) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	ctx, err := tag.New(cen.ctx, tag.Insert(census.kProfile, profile))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}
	stats.Record(ctx, cen.mSegmentSourceAppeared.M(1))
}

func SegmentUploaded(nonce, seqNo uint64, uploadDur time.Duration) {
	glog.V(logLevel).Infof("Logging SegmentUploaded... nonce=%d seqNo=%d dur=%s", nonce, seqNo, uploadDur)
	census.segmentUploaded(nonce, seqNo, uploadDur)
}

func (cen *censusMetricsCounter) segmentUploaded(nonce, seqNo uint64, uploadDur time.Duration) {
	stats.Record(cen.ctx, cen.mSegmentUploaded.M(1), cen.mUploadTime.M(float64(uploadDur/time.Second)))
}

func AuthWebhookFinished(dur time.Duration) {
	census.authWebhookFinished(dur)
}

func (cen *censusMetricsCounter) authWebhookFinished(dur time.Duration) {
	stats.Record(cen.ctx, cen.mAuthWebhookTime.M(float64(dur)/float64(time.Millisecond)))
}

func SegmentUploadFailed(nonce, seqNo uint64, code SegmentUploadError, reason string, permanent bool) {
	if code == SegmentUploadErrorUnknown {
		if strings.Contains(reason, "Client.Timeout") {
			code = SegmentUploadErrorTimeout
		} else if reason == "Session ended" {
			code = SegmentUploadErrorSessionEnded
		}
	}
	glog.Errorf("Logging SegmentUploadFailed... code=%v reason='%s'", code, reason)

	census.segmentUploadFailed(nonce, seqNo, code, permanent)
}

func (cen *censusMetricsCounter) segmentUploadFailed(nonce, seqNo uint64, code SegmentUploadError, permanent bool) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	if permanent {
		cen.countSegmentEmerged(nonce, seqNo)
	}

	ctx, err := tag.New(cen.ctx, tag.Insert(census.kErrorCode, string(code)))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}
	stats.Record(ctx, cen.mSegmentUploadFailed.M(1))
	if permanent {
		cen.countSegmentTranscoded(nonce, seqNo, true)
		cen.sendSuccess()
	}
}

func SegmentTranscoded(nonce, seqNo uint64, segDuration float64, transcodeDur time.Duration, profiles string) {
	glog.V(logLevel).Infof("Logging SegmentTranscode nonce=%d seqNo=%d dur=%s segment_duration=%v", nonce, seqNo, transcodeDur, segDuration)
	census.segmentTranscoded(nonce, seqNo, segDuration, transcodeDur, profiles)
}

func (cen *censusMetricsCounter) segmentTranscoded(nonce, seqNo uint64, segDuration float64, transcodeDur time.Duration,
	profiles string) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	ctx, err := tag.New(cen.ctx, tag.Insert(cen.kProfiles, profiles))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}
	transDurSec := transcodeDur.Seconds()
	now := time.Now()
	stats.Record(ctx, cen.mSegmentTranscoded.M(1), cen.mTranscodeTime.M(transDurSec))
	stats.Record(cen.ctx, cen.mTranscodeTimeAvg.M(cen.transcodeTimeAverage.addSample(now, transDurSec)))
	if segDuration > 0 {
		ratio := transDurSec / segDuration
		stats.Record(ctx, cen.mTranscodeTimeRealtimeRatio.M(ratio))
		stats.Record(cen.ctx, cen.mTranscodeTimeRealtimeRatioAvg.M(cen.transcodeTimeRatioAverage.addSample(now, ratio)))
	}
}

func SegmentTranscodeFailed(subType SegmentTranscodeError, nonce, seqNo uint64, err error, permanent bool) {
	glog.Errorf("Logging SegmentTranscodeFailed subtype=%v nonce=%d seqNo=%d error='%s'", subType, nonce, seqNo, err.Error())
	census.segmentTranscodeFailed(nonce, seqNo, subType, permanent)
}

func (cen *censusMetricsCounter) segmentTranscodeFailed(nonce, seqNo uint64, code SegmentTranscodeError, permanent bool) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	ctx, err := tag.New(cen.ctx, tag.Insert(census.kErrorCode, string(code)))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}
	stats.Record(ctx, cen.mSegmentTranscodeFailed.M(1))
	if permanent {
		cen.countSegmentEmerged(nonce, seqNo)
		cen.countSegmentTranscoded(nonce, seqNo, code != SegmentTranscodeErrorSessionEnded)
		cen.sendSuccess()
	}
}

func (cen *censusMetricsCounter) countSegmentTranscoded(nonce, seqNo uint64, failed bool) {
	if avg, ok := cen.success[nonce]; ok {
		avg.addTranscoded(seqNo, failed)
	}
}

func (cen *censusMetricsCounter) countSegmentEmerged(nonce, seqNo uint64) {
	if _, ok := cen.emergeTimes[nonce][seqNo]; ok {
		stats.Record(cen.ctx, cen.mSegmentEmerged.M(1))
		delete(cen.emergeTimes[nonce], seqNo)
	}
}

func (cen *censusMetricsCounter) sendSuccess() {
	stats.Record(cen.ctx, cen.mSuccessRate.M(cen.successRate()))
}

func SegmentFullyTranscoded(nonce, seqNo uint64, segDuration float64, profiles string, errCode SegmentTranscodeError) {
	census.lock.Lock()
	defer census.lock.Unlock()
	ctx, err := tag.New(census.ctx, tag.Insert(census.kProfiles, profiles))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}

	if st, ok := census.emergeTimes[nonce][seqNo]; ok {
		if errCode == "" {
			now := time.Now()
			latency := now.Sub(st).Seconds()
			avg := census.transcodeOverallLatencyAverage.addSample(now, latency)
			stats.Record(ctx, census.mTranscodeOverallLatency.M(latency))
			stats.Record(census.ctx, census.mTranscodeOverallLatencyAvg.M(avg))

			if segDuration > 0 {
				ratio := latency / segDuration
				stats.Record(ctx, census.mTranscodeOverallLatencyRealtimeRatio.M(ratio))
				stats.Record(census.ctx, census.mTranscodeOverallLatencyRealtimeRatioAvg.M(census.transcodeOverallLatencyRatioAverage.addSample(now, ratio)))
			}
		}
		census.countSegmentEmerged(nonce, seqNo)
	}
	if errCode == "" {
		stats.Record(ctx, census.mSegmentTranscodedAllAppeared.M(1))
	}
	failed := errCode != "" && errCode != SegmentTranscodeErrorSessionEnded
	census.countSegmentTranscoded(nonce, seqNo, failed)
	if !failed {
		stats.Record(ctx, census.mSegmentTranscodedUnprocessed.M(1))
	}
	census.sendSuccess()
}

func TranscodedSegmentAppeared(nonce, seqNo uint64, profile string) {
	glog.V(logLevel).Infof("Logging LogTranscodedSegmentAppeared... nonce=%d SeqNo=%d profile=%s", nonce, seqNo, profile)
	census.segmentTranscodedAppeared(nonce, seqNo, profile)
}

func (cen *censusMetricsCounter) segmentTranscodedAppeared(nonce, seqNo uint64, profile string) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	ctx, err := tag.New(cen.ctx, tag.Insert(cen.kProfile, profile))
	if err != nil {
		glog.Error("Error creating context", err)
		return
	}

	// cen.transcodedSegments[nonce] = cen.transcodedSegments[nonce] + 1
	if st, ok := cen.emergeTimes[nonce][seqNo]; ok {
		latency := time.Since(st)
		glog.V(logLevel).Infof("Recording latency for segment nonce=%d seqNo=%d profile=%s latency=%s", nonce, seqNo, profile, latency)
		stats.Record(ctx, cen.mTranscodeLatency.M(float64(latency/time.Second)))
	}

	stats.Record(ctx, cen.mSegmentTranscodedAppeared.M(1))
}

func StreamCreateFailed(nonce uint64, reason string) {
	glog.Errorf("Logging StreamCreateFailed... nonce=%d reason='%s'", nonce, reason)
	census.streamCreateFailed(nonce, reason)
}

func (cen *censusMetricsCounter) streamCreateFailed(nonce uint64, reason string) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	stats.Record(cen.ctx, cen.mStreamCreateFailed.M(1))
}

func newAverager() *segmentsAverager {
	return &segmentsAverager{
		segments: make([]segmentCount, numberOfSegmentsToCalcAverage),
		end:      -1,
	}
}

func StreamCreated(hlsStrmID string, nonce uint64) {
	glog.V(logLevel).Infof("Logging StreamCreated... nonce=%d strid=%s", nonce, hlsStrmID)
	census.streamCreated(nonce)
}

func (cen *censusMetricsCounter) streamCreated(nonce uint64) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	stats.Record(cen.ctx, cen.mStreamCreated.M(1))
	cen.success[nonce] = newAverager()
}

func StreamStarted(nonce uint64) {
	glog.V(logLevel).Infof("Logging StreamStarted... nonce=%d", nonce)
	census.streamStarted(nonce)
}

func (cen *censusMetricsCounter) streamStarted(nonce uint64) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	stats.Record(cen.ctx, cen.mStreamStarted.M(1))
}

func StreamEnded(nonce uint64) {
	glog.V(logLevel).Infof("Logging StreamEnded... nonce=%d", nonce)
	census.streamEnded(nonce)
}

func (cen *censusMetricsCounter) streamEnded(nonce uint64) {
	cen.lock.Lock()
	defer cen.lock.Unlock()
	stats.Record(cen.ctx, cen.mStreamEnded.M(1))
	delete(cen.emergeTimes, nonce)
	if avg, has := cen.success[nonce]; has {
		if avg.canBeRemoved() {
			delete(cen.success, nonce)
		} else {
			avg.removed = true
			avg.removedAt = time.Now()
		}
	}
	census.sendSuccess()
}

// TicketValueSent records the ticket value sent to a recipient for a manifestID
func TicketValueSent(recipient string, manifestID string, value *big.Rat) {
	census.lock.Lock()
	defer census.lock.Unlock()

	if value.Cmp(big.NewRat(0, 1)) <= 0 {
		return
	}

	ctx, err := tag.New(census.ctx, tag.Insert(census.kRecipient, recipient), tag.Insert(census.kManifestID, manifestID))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mTicketValueSent.M(fracwei2gwei(value)))
}

// TicketsSent records the number of tickets sent to a recipient for a manifestID
func TicketsSent(recipient string, manifestID string, numTickets int) {
	census.lock.Lock()
	defer census.lock.Unlock()

	if numTickets <= 0 {
		return
	}

	ctx, err := tag.New(census.ctx, tag.Insert(census.kRecipient, recipient), tag.Insert(census.kManifestID, manifestID))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mTicketsSent.M(int64(numTickets)))
}

// PaymentCreateError records a error from payment creation
func PaymentCreateError(recipient string, manifestID string) {
	census.lock.Lock()
	defer census.lock.Unlock()

	ctx, err := tag.New(census.ctx, tag.Insert(census.kRecipient, recipient), tag.Insert(census.kManifestID, manifestID))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mPaymentCreateError.M(1))
}

// Deposit records the current deposit for the broadcaster
func Deposit(sender string, deposit *big.Int) {
	stats.Record(census.ctx, census.mDeposit.M(wei2gwei(deposit)))
}

func Reserve(sender string, reserve *big.Int) {
	stats.Record(census.ctx, census.mReserve.M(wei2gwei(reserve)))
}

// TicketValueRecv records the ticket value received from a sender for a manifestID
func TicketValueRecv(sender string, manifestID string, value *big.Rat) {
	census.lock.Lock()
	defer census.lock.Unlock()

	if value.Cmp(big.NewRat(0, 1)) <= 0 {
		return
	}

	ctx, err := tag.New(census.ctx, tag.Insert(census.kSender, sender), tag.Insert(census.kManifestID, manifestID))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mTicketValueRecv.M(fracwei2gwei(value)))
}

// TicketsRecv records the number of tickets received from a sender for a manifestID
func TicketsRecv(sender string, manifestID string, numTickets int) {
	census.lock.Lock()
	defer census.lock.Unlock()

	if numTickets <= 0 {
		return
	}

	ctx, err := tag.New(census.ctx, tag.Insert(census.kSender, sender), tag.Insert(census.kManifestID, manifestID))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mTicketsRecv.M(int64(numTickets)))
}

// PaymentRecvError records an error from receiving a payment
func PaymentRecvError(sender string, manifestID string, errStr string) {
	census.lock.Lock()
	defer census.lock.Unlock()

	var errCode string
	if strings.Contains(errStr, "Expected price") {
		errCode = "InvalidPrice"
	} else if strings.Contains(errStr, "invalid already revealed recipientRand") {
		errCode = "InvalidRecipientRand"
	} else if strings.Contains(errStr, "invalid ticket faceValue") {
		errCode = "InvalidTicketFaceValue"
	} else if strings.Contains(errStr, "invalid ticket winProb") {
		errCode = "InvalidTicketWinProb"
	} else {
		errCode = "PaymentError"
	}

	ctx, err := tag.New(
		census.ctx,
		tag.Insert(census.kSender, sender),
		tag.Insert(census.kManifestID, manifestID),
		tag.Insert(census.kErrorCode, errCode),
	)
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mPaymentRecvErr.M(1))
}

// WinningTicketsRecv records the number of winning tickets received from a sender
func WinningTicketsRecv(sender string, numTickets int) {
	census.lock.Lock()
	defer census.lock.Unlock()

	if numTickets <= 0 {
		return
	}

	ctx, err := tag.New(census.ctx, tag.Insert(census.kSender, sender))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mWinningTicketsRecv.M(int64(numTickets)))
}

// ValueRedeemed records the value from redeeming winning tickets from a sender
func ValueRedeemed(sender string, value *big.Int) {
	census.lock.Lock()
	defer census.lock.Unlock()

	if value.Cmp(big.NewInt(0)) <= 0 {
		return
	}

	ctx, err := tag.New(census.ctx, tag.Insert(census.kSender, sender))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mValueRedeemed.M(wei2gwei(value)))
}

// TicketRedemptionError records an error from redeeming a ticket
func TicketRedemptionError(sender string) {
	census.lock.Lock()
	defer census.lock.Unlock()

	ctx, err := tag.New(census.ctx, tag.Insert(census.kSender, sender))
	if err != nil {
		glog.Fatal(err)
	}

	stats.Record(ctx, census.mTicketRedemptionError.M(1))
}

// SuggestedGasPrice records the last suggested gas price
func SuggestedGasPrice(gasPrice *big.Int) {
	census.lock.Lock()
	defer census.lock.Unlock()

	stats.Record(census.ctx, census.mSuggestedGasPrice.M(wei2gwei(gasPrice)))
}

// TranscodingPrice records the last transcoding price
func TranscodingPrice(sender string, price *big.Rat) {
	census.lock.Lock()
	defer census.lock.Unlock()

	floatWei, _ := price.Float64()
	stats.Record(census.ctx, census.mTranscodingPrice.M(floatWei))
}

// Convert wei to gwei
func wei2gwei(wei *big.Int) float64 {
	gwei, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(float64(gweiConversionFactor))).Float64()
	return gwei
}

// Convert fractional wei to gwei
func fracwei2gwei(wei *big.Rat) float64 {
	floatWei, _ := wei.Float64()
	return floatWei / gweiConversionFactor
}

func newMovingAverage(windowSize time.Duration, dataSize int) *movingAverage {
	return &movingAverage{
		windowSize: windowSize,
		data:       newRingArray(dataSize),
	}
}

func newRingArray(size int) *ringArray {
	return &ringArray{
		data: make([]timeValue, size),
		head: -1,
		tail: -1,
	}
}

func (ma *movingAverage) addSample(now time.Time, val float64) float64 {
	for ma.data.hasData() {
		if tt, _ := ma.data.getTail(); now.Sub(tt) >= ma.windowSize {
			ma.data.pop()
		} else {
			break
		}
	}
	ma.data.push(now, val)
	return ma.data.average()
}

func (ra *ringArray) push(time time.Time, value float64) {
	if ra.head == -1 && ra.tail == -1 {
		ra.head = 0
		ra.tail = 0
		ra.data[0] = timeValue{time: time, value: value}
		return
	}
	newHead := ra.head + 1
	if newHead == ra.tail || newHead == len(ra.data) && ra.tail == 0 {
		newData := make([]timeValue, cap(ra.data)*2)
		if ra.tail < ra.head {
			copy(newData, ra.data[ra.tail:ra.head+1])
			ra.head = ra.head - ra.tail
		} else {
			copy(newData, ra.data[ra.tail:len(ra.data)])
			copy(newData[len(ra.data)-ra.tail:], ra.data[:ra.head+1])
			ra.head = len(ra.data) - ra.tail + ra.head
		}
		ra.tail = 0
		ra.data = newData
	}
	ra.head++
	if ra.head == len(ra.data) {
		ra.head = 0
	}
	ra.data[ra.head] = timeValue{time: time, value: value}
}

func (ra *ringArray) pop() (time.Time, float64) {
	if ra.tail == -1 {
		var t time.Time
		return t, 0
	}
	tailVal := ra.data[ra.tail]
	if ra.tail == ra.head {
		ra.tail = -1
		ra.head = -1
		return tailVal.time, tailVal.value
	}
	ra.tail++
	if ra.tail >= len(ra.data) {
		ra.tail = 0
	}
	return tailVal.time, tailVal.value
}

func (ra *ringArray) hasData() bool {
	return ra.head != -1 && ra.tail != -1
}

func (ra *ringArray) getTail() (time.Time, float64) {
	if ra.tail != -1 {
		dp := ra.data[ra.tail]
		return dp.time, dp.value
	}
	var t time.Time
	return t, 0
}

func (ra *ringArray) average() float64 {
	if !ra.hasData() {
		return 0
	}
	if ra.head == ra.tail {
		return ra.data[ra.head].value
	}
	var acc, num float64
	i := ra.tail
	for {
		acc += ra.data[i].value
		num++
		i++
		if i == len(ra.data) {
			i = 0
		}
		if i == ra.head {
			acc += ra.data[i].value
			num++
			break
		}
	}
	return acc / num
}
