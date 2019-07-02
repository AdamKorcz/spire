package metricsservice

import (
	"context"
	"time"

	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/proto/spire/common/hostservices"
)

// Config for the metrics host service
type Config struct {
	Metrics telemetry.Metrics
}

type metricsService struct {
	cfg Config
}

// NewMetricsService create and return new Metrics Service
func NewMetricsService(cfg Config) hostservices.MetricsService {
	return metricsService{
		cfg: cfg,
	}
}

func (m metricsService) AddSample(ctx context.Context, req *hostservices.AddSampleRequest) (*hostservices.AddSampleResponse, error) {
	labels := convertLabels(req.Labels)
	m.cfg.Metrics.AddSampleWithLabels(req.Key, req.Val, labels)
	return &hostservices.AddSampleResponse{}, nil
}

func (m metricsService) EmitKey(ctx context.Context, req *hostservices.EmitKeyRequest) (*hostservices.EmitKeyResponse, error) {
	m.cfg.Metrics.EmitKey(req.Key, req.Val)
	return &hostservices.EmitKeyResponse{}, nil
}

func (m metricsService) IncrCounter(ctx context.Context, req *hostservices.IncrCounterRequest) (*hostservices.IncrCounterResponse, error) {
	labels := convertLabels(req.Labels)
	m.cfg.Metrics.IncrCounterWithLabels(req.Key, req.Val, labels)
	return &hostservices.IncrCounterResponse{}, nil
}

func (m metricsService) MeasureSince(ctx context.Context, req *hostservices.MeasureSinceRequest) (*hostservices.MeasureSinceResponse, error) {
	labels := convertLabels(req.Labels)
	m.cfg.Metrics.MeasureSinceWithLabels(req.Key, time.Unix(req.Time, 0), labels)
	return &hostservices.MeasureSinceResponse{}, nil
}

func (m metricsService) SetGauge(ctx context.Context, req *hostservices.SetGaugeRequest) (*hostservices.SetGaugeResponse, error) {
	labels := convertLabels(req.Labels)
	m.cfg.Metrics.SetGaugeWithLabels(req.Key, req.Val, labels)
	return &hostservices.SetGaugeResponse{}, nil
}

func convertLabels(inLabels []*hostservices.Label) []telemetry.Label {
	labels := make([]telemetry.Label, 0, len(inLabels))
	for _, inLabel := range inLabels {
		if inLabel != nil {
			labels = append(labels, telemetry.Label{
				Name:  inLabel.Name,
				Value: inLabel.Value,
			})
		}
	}

	return labels
}
