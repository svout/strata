# harness — local rules (Python)

- One file per attack scenario in `attacks/`, named after the threat-model number; each asserts the expected outcome and the control that fired.
- Benign traffic and training data come from the same generator, seeded, so runs are reproducible.
- Never weaken a control to make a scenario pass. A failing scenario is a finding — record it in the vault, not in a patched assertion.
- Results: `results.py` writes every table and figure from raw run output. Nothing in the dissertation is drawn by hand.
- Load profiles are k6 scripts under `bench/`; keep the machine spec in the output file.
