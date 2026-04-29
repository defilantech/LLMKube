# Resource constrained deployments guide

You might want to run your model on devices that are limited in resources, like
edge / IoT devices such as [Raspberry Pi](https://www.raspberrypi.com). If you do
so some specific tuning might be required to prevent your service to crash, OOM,
or experience latency drop.

## Limiting model concurrency

> :warning: this features is available on version 0.7.4+

One approach consists in limiting the concurrency of the service so that only
a fixed number of query can be executed in parallel. This can be achieved
through `parallelSlots` specification in your `InferenceService`, here is an
example limiting concurrency to one request at a time on a
[Gemma 4](https://ai.google.dev/gemma/docs/core/model_card_4) model:


```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: gemma-4-e4b-q8
  namespace: llmkube
  labels:
    model: gemma-4-e4b-q8
spec:
  modelRef: gemma-4-e4b-q8
  parallelSlots: 1
  replicas: 1
  runtime: llamacpp
  resources:
    cpu: "2"
    memory: "12Gi"
```

You can also achieve the same effect through CLI deployment using `parallel`
option:

```bash
llmkube deploy gemma-4-e4b-q8 --cpu 2 --memory 12Gi --parallel 1
```

The behavior would change depending on the runtime used:

| Runtime     | Behavior                          |
| ----------- | --------------------------------- |
| generic     | Not supported                     |
| llama-cpp   | Set `--parallel` CLI arg          |
| ollama      | Set `OLLAMA_NUM_PARALLEL` env var |
| omlx        | Not supported                     |
| personaflex | Not supported                     |
| tgi         | Not supported                     |
| vllm        | Set `--num-max-seqs` CLI arg      |
