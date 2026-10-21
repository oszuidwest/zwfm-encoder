# Fix-on-fix and dispersed guards: recording and the rest of the app

Analysis dated 22 October 2026. History is measured up to and including `3e82d0c` (#507). The code inventory and the gap tests run against `ccb54e1` (#508).

## Conclusion

The claim that recording is a problem area holds, but for a more precise reason than "fixes repair fixes".

1. **Literal regressions are rare.** In a sample of 16 fix-on-fix pairs found by SZZ, after a second rater and adjudication, only **1** is a fix that broke an earlier fix (Wilson 95%: 1–28%). The rest are a later fix for a different problem in the same lines (8) or mechanical changes (7).
2. **The weakness is incomplete fixes.** A fix repairs one path, and review then finds a second path that enforces the same rule in its own place. Almost every such follow-up was found during review before merge, not in production (22 of 24).
3. **The same pattern exists outside recording**, in three areas the history only half shows:
   - the **start/stop lifecycle** across encoder, streaming, recording and silencedump;
   - the **config→runtime path** in the API;
   - **alerting** (notify and eventlog). Here it is latent: barely visible in history, clearly visible in the code.
4. **Web, audio, dante and types do not have it.** Web has many fixes but a low lift (0.67). There is one real web regression: #337→#338.

The underlying cause is the same everywhere: **contracts that span components have no owner.** Think of "no start after Stop", "Stop waits for everything it started", "what is on disk is also running", "an incident that opens also closes" and "a file has exactly one owner". Each component solves them with its own mechanism, and each fix adds another enforcement point.

## What the original claim gets right, and where it is more nuanced

| Claim | Verdict | Evidence |
|---|---|---|
| "Many fixes repair earlier fixes" (recording) | **Holds, but mostly incomplete fixes, not regressions** | A strict rule gives 6 of 13 recording issues caused or left incomplete by the cited fix, versus 2 of 11 elsewhere. A broad rule gives 10 of 13 versus 4 of 11. The difference is suggestive but not significant (Fisher p≈0.1). |
| "PR history confirms the pattern" | **Partly: the history also measures the review process** | 53 of the 56 October issues are "Found during the review of #X". Up to 9 fix PRs were open at once on recording, 7 on encoder and 6 on root. All issues and fixes have a single author, and reviews ran in batches. |
| "Guards and upload logic are spread out" | **Holds, and is reproducible** | See the table below: 15 enforcement points for start admission, 17 for file ownership (with 3 different ownership tests), 4 containers for pending uploads of which the counter reads 1, and 8 writers of the status error. There are 5 failing tests. |
| (implicit) "recording is unique" | **False** | The same staleness problem is solved in 4 components with 4 different mechanisms. |

## Method

**Sources.**
- The full git history at line level, from the first Go commit: 401 commits, 136 of them fixes.
- All 363 merged PRs with their files, and all 138 issues with their bodies, via `gh`.
- The code at HEAD.
- The conventional-commit scope is **not** used, because ±95 of the 136 fixes have no scope. The module is derived from the paths.

**Techniques.**
- **SZZ:** identifies the commit that introduced a bug, by blaming the changed lines on the parent of each fix.
- **Base-rate lift:** corrects for the fact that code already written by fixes is touched more often.
- **Hotspots:** revisions × cognitive complexity.
- **Classification of issues** that cite an earlier fix.
- **Invariant inventory** per area.
- **Single-guard mutation tests.**
- **Backtest** at cut-off dates 1 June and 1 August.

**Adversarial check (codex).** Codex was used three times:
1. A blind replication from the definitions only, with its own script.
2. A blind second rating of the sample.
3. An instruction to refute every conclusion.

The definitions were fixed before the analysis in [`fix-patterns/definitions.txt`](fix-patterns/definitions.txt).

## Results per module

| Module | Fixes | Fix-on-fix (SZZ, module-local) | Lift vs. base rate | Current code written by fix commits | Fix PRs later cited in a bug issue |
|---|---:|---:|---:|---:|---:|
| recording | 30 | 19 / 28 | 1.17 | **65%** (52% without the 44-file squash 20dee08) | **18 / 27** |
| encoder | 27 | 7 / 17 | 0.57 | 41% | 3 / 11 |
| web | 43 | 9 / 38 | 0.67 | 13% (JS) | 0 / 26 |
| root (api/server) | 21 | 3 / 18 | 0.30 | 18% | 4 / 9 |
| config | 19 | 5 / 15 | 0.26 | 39% | 0 / 9 |
| notify | 18 | 5 / 16 | **2.06** | 16% | 1 / 10 |
| streaming | 9 | 4 / 8 | 1.07 | 31% | 0 / 3 |
| eventlog | 11 | 1 / 8 | 0.20 | 25% | 1 / 4 |
| server | 8 | 1 / 6 | 0.53 | 57% | 0 / 3 |
| dante | 5 | 1 / 4 | 1.97 | 23% | 0 / 5 |

**Reading the table:**
- **Lift** = (share of blamed lines that come from a fix) ÷ (share of all lines in those files that come from a fix). Recording scores 1.17: its code is already largely fix code, so every change naturally touches fix lines. Notify scores 2.06, but that comes from a January cluster around Graph and silence (bbea16c → ece55cf → e98de81). Notify also had five fixes in October; the area is not quiet.
- **Recording's fix share** comes for one fifth from a single squash commit. That commit (20dee08, #481 with #482 stacked on it) contains new functionality under a `fix` label. Without it, recording is still the highest.
- **Last column:** the module is the one containing most of the PR's changed source files. Counting every touched module instead, 3 web fix PRs are also cited. Those reports concern the non-web part of those PRs.
- **Replication:** codex and I independently get the same 401/136/117/51, the same 99 pairs and the same lifts to three decimals.

## Dispersed guards per area

### Recording (confirms the claim)
- **Start admission:** 15 enforcement points. Five manager methods copy the running/registered check, and `startHourly`/`startOnDemand` plus `manager.go:495` copy the enabled/mode check. `GenericRecorder.Start` bypasses all of them; it has no production caller (#515).
- **File ownership:** 17 enforcement points with three different ownership tests:
  - filename prefix: `cleanup.go:87`, `spool.go:203`;
  - timestamp only: `cleanup.go:179`;
  - `RecorderID` in metadata, where an empty ID is accepted: `spool.go:98`.

  Spool directories use the raw ID, file prefixes the hex-encoded one (#495).
- **Pending uploads:** an upload is spread across `uploadQueue`, `retryQueue`, `retryInFlight` and on-disk metadata. `PendingUploadCount` reads only `retryQueue` (#488). `retryQueue` is appended to in three places and `savePendingUpload` has six callers. "Deferred, not failed" is a string sentinel written in four places.
- **Deleting a file:** this happens in 9 places. Retention (`cleanup.go:87-106`) ignores upload state, so **in "both" mode retention deletes a local file whose S3 upload is still pending.** This was not reported before.
- **Status error:** 4 writers and 4 clearers, with no owner. `stopLocked` clears a finalize error that was just set (#514).
- **Duplicated transitions:** the step from finalizing to quarantined/empty exists twice (`failOutput` and `finishEncoder`).
- **Mutations:** 10 of ~50 guard mutations survive the suite.

### Lifecycle across components (encoder, streaming, recording, silencedump, srtfanout)
- **"No consumer start after Stop"** has about 30 enforcement points.
- **"Is this async start still current?"** is answered differently per component:
  - a counter (`sourceRunID`, `startGen`, `rotationGeneration`, `durationGeneration`);
  - pointer identity (`*Stream`, `*encoderRun`);
  - channel identity (`stopChan`, `cleanupStopCh`);
  - booleans (`running` in recording/silencedump, `started`/`stopping` in srtfanout).
- **Joining a Stop already in progress** works in encoder and recorder, but not in streaming (`streaming/manager.go:605-608`).
- **Stop does not wait for:** the delayed starter, the `RestartStream` goroutine, `MonitorAndRetry`, silence-dump encodes (#494) and the "stable" sleeper.
- **Duplicated state:** "running" is stored in 4 places, "Stop began" in 3, "current run" in 2 (under two different mutexes).
- **Mutations:** the ownership checks in streaming (`manager.go:271`, `:338`, `:1204`) and the encoder checks (`:917-921`, `:1045`, `:672`) can all be removed without any test failing.

### Config → runtime (api.go, config, encoder)
- **"If it is on disk, apply it"** is written out three times, for recorders only (`encoder.go:1282,1299,1315`). It is missing for streams, settings and the API key.
- **The order differs per type.** A recorder is saved first and stopped second; a stream is stopped first and saved second (#492).
- **Validation is shared, the error code is not.** The config layer returns an unwrapped error, which `writeConfigError` reports as a 500. Only the copy in the handler turns it into a 400; without that copy it becomes a 500, and no test notices.
- **The `..` check on `local_path`** exists only in the HTTP handlers, not in the config layer.
- **Secrets:** three different mechanisms to keep, clear or replace them on update. The test endpoints ignore clear flags.
- **Mutations:** 11 runtime calls and guards survive, including all three stream runtime calls and the teardown when a recorder is removed.

### Alerting (latent: same disease, not yet in the history)
- **Each alarm type implements open/close itself.** There is no shared incident mechanism.
- **There are two fan-out paths.** Input and storage are delivered in order per channel and ignore subscriptions. Silence, imbalance, dump and upload use one goroutine per message, unordered.
- **Silence and imbalance** do not survive a restart. `Reset()` (on every settings save) does not close an open incident, so a second start alert follows and Zabbix stays on DISASTER.
- **The Zabbix "Upload Abandoned" trigger** cannot recover.
- **Retry exists only for Graph mail.** Webhook and Zabbix have none, and a lost recovery is never resent.

### Deploy, web, other
- **install.sh:** #510 already existed before #505 (the line was unchanged), so deploy is not a fix-on-fix area.
- **Web:** one real regression (#337→#338). Otherwise no pattern.

## Gap tests

The tests are behind the `audit` build tag, so the normal suite stays green.

```
go test -tags audit -run TestAudit ./ ./internal/recording/ ./internal/encoder/ ./internal/silencedump/ ./internal/notify/
```

All ten fail on `ccb54e1`:

| Test | Area | Fails because |
|---|---|---|
| `TestAuditFinalizeFailureDuringStopStaysInStatus` | recording | Status is empty after a finalize failure in Stop (#514) |
| `TestAuditPendingUploadCountDuringRetryBatch` | recording | The pending counter reports 0 during a retry batch (#488) |
| `TestAuditRetentionKeepsFileWithPendingUpload` | recording | Retention deletes a file with a pending upload (**new**) |
| `TestAuditStoppedRecorderReleasesCurrentFile` | recording | A stopped recorder keeps `currentFile`, so retention skips that file (**new**) |
| `TestAuditGenericRecorderStartHonorsDisabled` | recording | `Start()` bypasses the guards (#515, unreachable in production) |
| `TestAuditStreamDisabledDuringInputOutageStops` | lifecycle | A stream disabled during an input outage keeps FFmpeg running (**new**) |
| `TestAuditStopWaitsForRunningEncode` | lifecycle | Stop returns while a dump encode is still running (#494) |
| `TestAuditRegenerateKeyCommittedSaveReturnsLiveKey` | config→runtime | The key is live, but the response does not return it (**new**) |
| `TestAuditSilenceEndNeverOvertakesSlowSilenceStart` | alerting | The recovery overtakes the start (**new**) |
| `TestAuditResetDuringSilenceKeepsStartEndAlternating` | alerting | After a Reset, start follows start without an end (**new**) |

## Consolidation designs (no compatibility constraints)

### 1. Recording: one ledger per file
- **Replace:** marker files, `pendingUpload` metadata, the abandoned flags (3 copies), `uploadQueue`, `retryQueue`, `retryInFlight` and the string sentinels.
- **With:** one `fileLedger` holding one persistent record per file with explicit states: `Reserved → Recording → Finalizing → {Quarantined | Empty | Finalized} → {KeptLocal | Queued → Uploading → {Uploaded | Pending → Abandoned}} → Deleted`.
- **Transitions** go only through `ledger.Transition(file, from, to)`, which checks that the transition is allowed and persists it before any side effect.
- **Retention and deletion** ask the ledger. A file can only be deleted in `Uploaded`, `Empty`, or `KeptLocal` beyond the retention period.
- **`PendingUploadCount`** becomes `ledger.Count(Queued, Uploading, Pending)`.
- **Status error:** derived from the last failed transition of the current run, instead of four writers and four clearers.
- **Remove:**
  - `GenericRecorder.Start`;
  - the legacy identity (`LegacyFilePrefix`, `IDBasedFiles`, the migration in `types.go:469-476` and `config.go:614`);
  - the name-prefix rule (#433), since names no longer affect files;
  - the case-sensitive raw ID in spool paths. Use the encoded ID everywhere.
- **Tests:** one table-driven test that walks the full transition table, plus a property test that generates random sequences of start/stop/rotate/upload-fail/restart. After every step: each file has exactly one state, no file in `Pending` has disappeared from disk, and the counter equals the number of records in `Queued`, `Uploading` and `Pending`.

### 2. Lifecycle: one `Run` primitive for all components
- **New package** `internal/lifecycle`, with `owner.Begin() (*Run, error)` (refused while the owner is stopping), `run.Go(fn)` (tracked), `run.Valid()`, `run.Context()` and `owner.Stop(ctx)`.
- **Stop** cancels the run and waits for every `run.Go` function. A second Stop joins the first.
- **Replaces:**
  - `sourceRunID`, `lifecycleGeneration`, the `restarting` flags and `restartGate` in encoder;
  - pointer identity and `errStoppedByUser` in streaming;
  - `startGen`, `rotationGeneration`, `durationGeneration` and `running` in recording;
  - `running` in silencedump;
  - `started`, `stopping` and `stopOnce` in srtfanout.
- **Enforcement:** the ~30 copies of "may I still start?" become one `run.Valid()` at the moment of publishing.
- **Timers and async work** (delayed starter, `RestartStream`, `MonitorAndRetry`, dump encodes, stable sleeper) run through `run.Go`. That removes #494 and the unjoined goroutines in one go.
- **Tests:** `goleak` after every lifecycle test, plus one shared contract test run against every component: stop during start, start during stop, double stop, and an async callback after stop.

### 3. Config → runtime: one transaction path
- **One function** `ApplyChange(change)` for stream, recorder, settings and key. The order is fixed: validate (typed `ValidationError` → always 400) → persist → apply to runtime → combined result.
- **A committed save** (`ErrSaveCommitted`) counts as success with a warning everywhere, never again as a 500 without data.
- **Remove:** the validation copies in the handlers (`api.go:325`, `:383`, `:507`, `:571`), the handler-only path checks (they move into `Recorder.Validate`), "stop first, then save" for streams, and two of the three secret mechanisms. What remains is one `SecretField{Keep|Clear|Set}`, also used by the test endpoints.
- **Tests:** one table test over every kind of change (stream, recorder, settings, key) × every outcome (valid, invalid, save failed, committed, runtime failed). Each cell states what is on disk, what is running and which HTTP status is returned.

### Latent fourth: one incident model for alerting
- **One incident type** for all alarm types: `Incident{Kind, Key, ID}`, with open/close state in the event log (so it survives a restart).
- **Delivery:** the same ordered per-channel FIFO for all types.
- **`Reset()`** explicitly closes open incidents.
- **Zabbix:** every alarm gets a recovery expression.

## Verification, and what the checks showed

- **Replication:** codex's own script arrives at exactly the same counts and pairs. The only difference is attribution per module: codex attributes per commit, I attribute per module, and per module is cleaner.
- **Second rater:** a blind classification of 16 pairs gives κ = 0.41. I had 4 regressions; codex rejected 3 of them with evidence (the S3 drain dates from 53a6a942/c1020df1, before both supposed causes). After adjudication 1 remains.
- **Backtest:** it **failed** for the historical fix-on-fix metric (top 4 correct: 0–2), which does no better than module size. A concurrency count looked better (2–3 of 4), but nothing is left after controlling for LOC (partial ρ 0.16 and −0.07). Both cut-off dates also fall largely on the same October wave. **There is no validated predictive metric.** The ranking above is descriptive.
- **Test runner:**
  - Ten tests fail on `main`, and the suite without the tag is green.
  - Two surviving mutants were reproduced by hand:
    - the ownership checks in streaming (`manager.go:338` and the listener variant);
    - `recorder.go:471`.
- **Counter-evidence accepted from codex:**
  - #510 already existed before #505.
  - Web does have one regression.
  - Notify was not quiet in October.
  - The concurrency indicator does not hold up.
  - The recording tests are not "per bug only": invariant tests exist (`TestStaleStartValidationCannotChangeStoppedOrNewRun`, `TestCommittedUploadWithLostResponseCreatesOneObject`). The size of the test suite therefore explains nothing.
  - `lifecycle_test.go:33` deliberately holds `retryProcessMu` forever ("Never unlock"). A test that suppresses a goroutine this way is a symptom of the missing join that design 2 addresses.

## Limitations

- **One author, one workflow.** All issues and fixes come from the same maintainer, and reviews ran in batches. Modules that get more review attention get more issues, without more going wrong in production. A denominator for that review exposure is missing.
- **The issue classification is a judgement.** Under both a strict and a broad rule recording comes out higher, but the difference is not significant.
- **Squashed and stacked PRs** distort what counts as a "fix commit": #482 reached main through #481.
- **Untested, read only:**
  - a spool-path escape via a hand-edited recorder ID containing `../`;
  - a late error landing in a restarted run;
  - a duplicate retry batch when an ID is reused.

## Side finding

- **SRT passphrase in the error status and event log (not verified).** The last FFmpeg stderr line becomes the stream error unfiltered and reaches the event log that way (`streaming/manager.go`, `util/errors.go:21`). The SRT URL with the passphrase is in the FFmpeg arguments (`streaming/ffmpeg.go:56-61`). Whether FFmpeg puts that URL on its last line was not checked.

## Reproduction

Scripts and raw classifications are in [`fix-patterns/`](fix-patterns/):

```
python3 fix-patterns/szz.py <repo> out/                          # per_module.csv, pairs.csv, hotspots.csv, fixes.json
python3 fix-patterns/szz.py <repo> bt-0601/ --until 2026-06-01   # backtest snapshots (same for 0801)
python3 fix-patterns/conc.py <worktree@0601> <worktree@0801> <repo>
python3 fix-patterns/backtest.py                                 # expects mine/, bt-0601/, bt-0801/ in the working directory
```

`GOCOGNIT` points to the `gocognit` binary (v1.2.0).
