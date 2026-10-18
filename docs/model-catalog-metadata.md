# Model catalog metadata

`makewand models --json` retains the existing `available` labels and adds
`candidates`: tool, selected account reference, model ID, model family, actual
model, quota pool, source, cache time, and call-ID verification state.
Account references are non-secret hashes; absent identities, pools and observations
remain null. `actual_model` remains null for prospective catalog candidates;
per-attempt actual-model observations are stored in execution evidence. A catalog
label or configured model is not an execution observation.

Antigravity catalogs retain Gemini, Claude, GPT and unknown entries such as Argon.
Each entry carries its cache-file source and UTC file modification time. Native
Gemini IDs retain their existing automatic routing behavior. Other IDs remain
visible but are excluded from automatic tier selection, including configured
defaults, even after successful CLI execution evidence confirms the actual ID.
Execution verifies an ID, not whether it spends credits; automatic selection
keeps master's native-Gemini-only policy and every agy launch still enforces
`useG1Credits=false`. Failed, unknown and API attempts do not verify a
subscription CLI ID. `--model <id>` is
an explicit per-call override and does not itself verify the ID. No catalog
lookup makes a model call to validate an ID.

Review routing compares model families across tools, including fallback review,
repair review and race approval. Claude through Antigravity and Claude Code share
one family. Unknown families cannot establish independence. The author's tier
and explicit model override matter; the review runs at the deep tier. For auto
pacing, independence must hold across all possible implementation tiers. Explicit
low-risk single-tool workflows retain their existing self-review policy.

Claude tier selections include scores and selected/rejected explanations in the
JSON catalog and human output, including Opus. Choose a tier with `--tier
fast|standard|deep` or override it with `--model opus` / a full model ID on the
existing Claude and pipeline commands. The override takes precedence.

Antigravity quota is unknown, with its source and last status observation time,
even when its CLI is healthy. Neither health nor catalog discovery establishes
100% remaining quota or an unlimited allowance. A recorded health failure still
prevents dispatch; an unknown percentage is not an exhausted allowance.
