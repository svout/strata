# control-plane — local rules (Python)

- Never on the hot path. If the gateway would have to wait for this service during a payment, the design is wrong.
- `ml/features.py` is the single definition of every feature. Changing it means updating the Go mirror and the parity test in the same step.
- Training is a script, not a notebook: seeded, logged, reproducible, and it writes the model plus its metrics and version to disk.
- Export models to ONNX; the gateway loads by version. Never ship a pickle.
- Pydantic models on every endpoint; no untyped dicts.
- Key material: generate and store test keys only, never anything reusable elsewhere; revocation is a first-class operation, not a delete.
- Tests: `pytest`, with `hypothesis` for policy evaluation edge cases.
