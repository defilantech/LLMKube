package metrics

var phase = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "llmkube_inferenceservice_phase"}, nil)
