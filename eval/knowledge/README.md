# Knowledge evaluation

Run the deterministic Go/TypeScript/Swift corpus:

```
go test ./internal/knowledge -run 'TestKnowledgeGoldenQuestions|TestWarmRetrievalTwoThousandFiles' -v
```

`questions.json` contains ten intent questions. The test creates equivalent
source fixtures in three languages, yielding 30 retrieval cases. The expected
module must appear in the first three results for at least 27 cases. It checks
freshness and the 8 KB search / 12 KB context JSON limits. This is a controlled
retrieval corpus, not evidence of broad software-engineering task success.

The performance fixture adds 2,000 small Go files and measures 20 warm searches;
p95 must remain under 250 ms. File sizes, hardware and filesystem cache matter.
Race tests should use `-short` to exclude this disk-intensive timing test; the
normal suite still runs it.

A real-provider paired experiment is explicitly opt-in:

```
PALLIUM_AGENT_EVAL_REPO=/path/to/indexed/disposable/repo \
  go test ./internal/knowledge -run TestPairedAgentEvaluation -v
```

It makes four provider calls, counted against the shared 40-call knowledge cap.
Two tasks compare legacy full documents against compact context, allowing source
reads in both conditions. Raw answers, input bytes and elapsed milliseconds are
written to the system temporary directory as `pallium-paired-agent-results.json`.
Provider token/cost and file-read counts are unavailable through this text interface;
do not infer them from bytes or report them as zero.

Observed development run (Pallium v0.9.23 source, installed Codex provider):

| Task | Legacy input | Compact input | Legacy elapsed | Compact elapsed | Source answer |
|---|---:|---:|---:|---:|---|
| Stop a loop | 63,642 B | 2,735 B | 20.9 s | 20.9 s | Both correct |
| Update installation | 68,981 B | 2,646 B | 19.3 s | 25.7 s | Both correct |

The context payload fell by about 96%. No speed improvement was demonstrated.
The initial stop-loop harness expected `loop_runtime.go`; source inspection showed
that `cmd/loop.go:runLoopStop` and `loop_store.go:SetLoopStatus` are the correct
implementation. Both answers named those files and correctly distinguished stopping
future ticks from cancelling a running tick. The expected path was corrected;
this was a harness correction, not an improvement to the answers.
