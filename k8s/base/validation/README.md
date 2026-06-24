# Validation helpers

`model-prepull-job.yaml` is optional and intentionally not included in the base kustomization because clusters differ in RBAC policy. Apply it only after granting the job's service account permission to list pods and exec into Ollama pods in `ai-local`.
