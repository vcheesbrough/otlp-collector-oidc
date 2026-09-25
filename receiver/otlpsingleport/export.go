package otlpsingleport

import (
	"context"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
)

// dataFormatProtobuf is the format the stock receiver reports for every
// request: both encodings decode to the same pdata.
const dataFormatProtobuf = "protobuf"

// tracesExporter hands one decoded traces request to the next consumer and
// reports the outcome. One exists per transport, so the counters say which.
type tracesExporter struct {
	ptraceotlp.UnimplementedGRPCServer

	next consumer.Traces
	obs  *receiverhelper.ObsReport
}

// Export implements ptraceotlp.GRPCServer.
func (e *tracesExporter) Export(ctx context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	td := req.Traces()
	n := td.SpanCount()
	if n == 0 {
		return ptraceotlp.NewExportResponse(), nil
	}
	ctx = e.obs.StartTracesOp(ctx)
	err := e.next.ConsumeTraces(ctx, td)
	e.obs.EndTracesOp(ctx, dataFormatProtobuf, n, err)
	if err != nil {
		return ptraceotlp.NewExportResponse(), statusFromConsumerError(err)
	}
	return ptraceotlp.NewExportResponse(), nil
}

// logsExporter is tracesExporter for logs.
type logsExporter struct {
	plogotlp.UnimplementedGRPCServer

	next consumer.Logs
	obs  *receiverhelper.ObsReport
}

// Export implements plogotlp.GRPCServer.
func (e *logsExporter) Export(ctx context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	ld := req.Logs()
	n := ld.LogRecordCount()
	if n == 0 {
		return plogotlp.NewExportResponse(), nil
	}
	ctx = e.obs.StartLogsOp(ctx)
	err := e.next.ConsumeLogs(ctx, ld)
	e.obs.EndLogsOp(ctx, dataFormatProtobuf, n, err)
	if err != nil {
		return plogotlp.NewExportResponse(), statusFromConsumerError(err)
	}
	return plogotlp.NewExportResponse(), nil
}

// metricsExporter is tracesExporter for metrics.
type metricsExporter struct {
	pmetricotlp.UnimplementedGRPCServer

	next consumer.Metrics
	obs  *receiverhelper.ObsReport
}

// Export implements pmetricotlp.GRPCServer.
func (e *metricsExporter) Export(ctx context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	md := req.Metrics()
	n := md.DataPointCount()
	if n == 0 {
		return pmetricotlp.NewExportResponse(), nil
	}
	ctx = e.obs.StartMetricsOp(ctx)
	err := e.next.ConsumeMetrics(ctx, md)
	e.obs.EndMetricsOp(ctx, dataFormatProtobuf, n, err)
	if err != nil {
		return pmetricotlp.NewExportResponse(), statusFromConsumerError(err)
	}
	return pmetricotlp.NewExportResponse(), nil
}
