# VRAM Budget Router Design

## Status

Design proposal for the `llama-swap-vrambudget` fork.

The goal is to add a memory-budget-based eviction strategy while keeping the fork easy to synchronize with upstream `mostlygeek/llama-swap`.

This document intentionally favors additive changes, small integration points, and compatibility with the existing router/scheduler architecture.

## Problem statement

The existing group and matrix routers decide which models may coexist from predefined groups or matrix sets. That is useful for explicit placement policies, but it does not provide the desired behavior for a machine with a fixed GPU or unified-memory budget:

1. Keep any combination of models loaded while the configured memory budget is not exceeded.
2. When a requested model would exceed the budget, evict idle models until enough memory is available.
3. Prefer the model that has been idle for the longest time.
4. Never interrupt an active request.
5. If sufficient memory can only be freed by models that are still serving requests, queue the new request and retry when those requests finish.
6. Do not require an exhaustive list of valid model combinations.

Runtime memory measurement is explicitly out of scope for the first implementation. Per-model memory values are supplied in configuration.

## Upstream-sync strategy

The fork should avoid broad rewrites of upstream code. The implementation should follow these rules:

- Add a new router/swapper instead of changing matrix semantics.
- Keep the existing `scheduler.Swapper` interface unchanged.
- Keep the existing FIFO scheduling behavior unchanged for group and matrix routers.
- Add optional lifecycle hooks through a separate interface and invoke them via type assertions.
- Store nearly all new logic in new files.
- Keep edits to shared upstream files short and mechanical.
- Do not add memory fields to `ModelConfig` initially; use existing arbitrary model metadata.
- Do not modify process lifecycle code unless a concrete bug requires it.
- Add tests around the new code rather than altering existing test expectations.

This structure should make upstream merges mostly additive and limit recurring conflicts to router registration, configuration wiring, and a few optional scheduler hook calls.

## Proposed configuration

```yaml
models:
  qwen3.5-122b:
    cmd: >-
      ...
    metadata:
      projected_total_mib: 106724

  qwen3.6-35b-a3b:
    cmd: >-
      ...
    metadata:
      projected_total_mib: 37167

routing:
  router:
    use: budget
    settings:
      budget:
        total_mib: 240000
        reserve_mib: 8000
        memory_metadata_key: projected_total_mib
        eviction:
          policy: lru
          evict_costs:
            qwen3.5-122b: 100
            qwen3.6-35b-a3b: 10
```

Effective capacity is:

```text
effective_budget_mib = total_mib - reserve_mib
```

`reserve_mib` protects the operating system, Metal/runtime overhead, KV-cache variation, and estimation error.

### Validation rules

Configuration loading must fail with a clear error when:

- `total_mib <= 0`
- `reserve_mib < 0`
- `reserve_mib >= total_mib`
- a configured model has no memory value
- a memory value is not a positive integer
- a single model is larger than the effective budget
- an eviction cost is negative
- an unknown eviction policy is configured

A model that is not managed by the budget router must not influence its accounting.

## New components

Prefer the following new files:

```text
internal/router/budget.go
internal/router/budget_swapper.go
internal/router/budget_solver.go
internal/router/budget_activity.go
internal/router/budget_test.go
internal/router/budget_solver_test.go
```

Configuration types may live in a new file such as:

```text
internal/config/budget_config.go
```

Documentation and example configuration should be added without replacing upstream examples.

## Budget router

The budget router should follow the same construction pattern as the existing matrix router:

1. Parse and validate the budget settings.
2. Extract per-model memory estimates from `ModelConfig.Metadata`.
3. Construct one process per model using the existing process machinery.
4. Construct a `budgetSwapper`.
5. Pass that swapper to the existing base router and FIFO scheduler.

No new process implementation is required.

Conceptually:

```go
func NewBudget(conf config.Config, proxylog, upstreamlog *logmon.Monitor) (*Budget, error) {
    settings, err := loadBudgetSettings(conf)
    if err != nil {
        return nil, err
    }

    swapper := newBudgetSwapper(settings, proxylog)

    // Reuse the same base-router and process construction pattern as the
    // existing routers.
    // ...
}
```

## Keep the existing `Swapper` interface

Do not change this upstream interface:

```go
type Swapper interface {
    EvictionFor(target string, running []string) []string
    OnSwapStart(target string, running []string)
}
```

Changing it would force edits in every router and increase merge conflicts.

Instead, add a fork-specific optional interface in a new file:

```go
type ActivityAwareSwapper interface {
    OnModelReady(modelID string, at time.Time)
    OnServeStart(modelID string, at time.Time)
    OnServeDone(modelID string, at time.Time)
    OnModelStopped(modelID string, at time.Time)
}
```

The FIFO scheduler can call these hooks only when the configured swapper implements the interface:

```go
if tracker, ok := s.planner.(ActivityAwareSwapper); ok {
    tracker.OnServeStart(modelID, time.Now())
}
```

Existing group and matrix swappers remain unaffected.

To reduce merge conflicts further, put hook dispatch in small helper functions in a new file where possible:

```go
func notifyServeStart(planner Swapper, modelID string, at time.Time) {
    if tracker, ok := planner.(ActivityAwareSwapper); ok {
        tracker.OnServeStart(modelID, at)
    }
}
```

Then shared scheduler edits are only one-line calls at stable lifecycle points.

## Activity tracking and LRU definition

The primary eviction criterion is longest idle time.

Define `idleSince` as the moment the model's last active request completed, not the moment the request started.

State per model:

```go
type modelActivity struct {
    readyAt       time.Time
    lastServeAt   time.Time
    lastDoneAt    time.Time
    active        int
    stopped       bool
}
```

Rules:

- `OnModelReady`: if the model has never served a request, set its initial idle time to the ready timestamp.
- `OnServeStart`: increment `active`; record `lastServeAt`.
- `OnServeDone`: decrement `active`; when it reaches zero, set `lastDoneAt` to the completion timestamp.
- `OnModelStopped`: remove or reset the activity entry.
- A model with `active > 0` is busy.
- For an idle model, use `lastDoneAt` when available, otherwise `readyAt`.
- Older `idleSince` means a stronger eviction preference.

Use an injected clock in tests:

```go
type Clock interface {
    Now() time.Time
}
```

Production uses a real clock; tests use a deterministic fake clock.

## Scheduler integration points

Only small additions should be needed in the existing FIFO scheduler.

### Request granted

After `GrantServe` succeeds and the in-flight counter is incremented:

```go
notifyServeStart(s.planner, modelID, s.clock.Now())
```

### Request finished

In `OnServeDone`, after decrementing the in-flight counter:

```go
notifyServeDone(s.planner, ev.ModelID, s.clock.Now())
```

### Model became ready

After a successful swap completes, before or when waiters are granted:

```go
notifyModelReady(s.planner, ev.ModelID, s.clock.Now())
```

This hook must be emitted once per transition to ready. If the current scheduler callback cannot distinguish a no-op readiness check from a real start, emitting it on every successful swap is acceptable provided the tracker does not reset the idle timestamp of an already-ready model.

### Model stopped

The preferred integration point is the code path that completes process stopping. If that requires invasive process changes, the first version may reset activity when a model is selected for eviction or explicitly unloaded. Document the approximation and add the exact stop hook later.

## Existing wait behavior should be reused

The FIFO scheduler already queues a request when its eviction set contains a model with active in-flight requests. It retries the queue after `OnServeDone`.

The budget implementation should therefore return the correct eviction set even when one or more selected victims are busy. The scheduler will wait rather than interrupt them.

Required behavior:

```text
request target
  -> calculate required memory
  -> choose eviction set
  -> if selected victim is busy, FIFO queues request
  -> victim finishes request
  -> OnServeDone drains queue
  -> solver runs again with current state
  -> idle victim is stopped
  -> target is loaded
```

Do not add a second queue inside `budgetSwapper`.

## Memory accounting

For a target model and the scheduler-provided running set:

```text
used_mib = sum(memory_mib[running models])
additional_mib = 0 if target is already running, otherwise memory_mib[target]
required_free_mib = used_mib + additional_mib - effective_budget_mib
```

If `required_free_mib <= 0`, return no evictions.

The running set can include targets of swaps already committed but not yet visible in process state. Treat all entries as consuming their configured full memory estimate. This is conservative and avoids parallel swaps overcommitting the budget.

Deduplicate model IDs before summing.

Unknown memory values should never reach the solver after successful configuration validation. If they do, fail conservatively and log an explicit error rather than assuming zero.

## Eviction solver

Only currently running models are candidates. The requested target is never a candidate.

Because the number of simultaneously loaded models is normally small, evaluate all candidate subsets rather than relying on a fragile greedy algorithm.

For `n` running candidates, this requires `2^n` subsets. This is acceptable for typical values of `n`. Add a documented fallback for unusually large `n`, for example a deterministic greedy strategy above 20 candidates.

A valid subset must free at least `required_free_mib`.

### Ranking

Rank valid subsets lexicographically in this order:

1. Prefer subsets containing only idle models.
2. Prefer the subset whose oldest-idle victims have been idle longest.
3. Prefer lower total configured eviction cost.
4. Prefer fewer evicted models.
5. Prefer less excess memory freed.
6. Use model ID ordering as a deterministic final tie-breaker.

LRU should be the dominant normal-use criterion, but active models require special treatment.

### Busy models

The solver may include busy models only when no idle-only subset can free enough memory.

This rule ensures:

- Idle models are always preferred when they can satisfy the request.
- A request can still make progress when the only sufficient victim is busy.
- The existing FIFO scheduler will queue the request until the busy victim becomes idle.

When busy victims are unavoidable, apply the same ranking using their last completed-idle timestamp or last activity timestamp, then configured eviction cost.

### Suggested comparison structure

```go
type evictionPlan struct {
    victims          []string
    includesBusy     bool
    idleOrder        []time.Time
    totalEvictCost   int
    freedMiB         int
    excessMiB        int
}
```

Sort `idleOrder` from oldest to newest and compare it lexicographically. This makes the longest-idle model the strongest LRU signal while still allowing the solver to choose a single sufficiently large model over many recently used models when appropriate tie-breakers apply.

The exact ranking must be covered by table-driven tests.

## Logging

`EvictionFor` must remain side-effect-free and should not log speculative decisions.

Log only in `OnSwapStart`, following the existing swapper convention.

Example:

```text
budget: model=qwen3.6-35b-a3b used=225167MiB target=37167MiB budget=232000MiB required_free=30334MiB evict=[qwen3-next-80b-a3b-thinking] freed=64167MiB policy=lru
```

Useful fields:

- target model
- effective budget
- configured target size
- accounted running memory
- required memory to free
- selected victims
- memory freed
- whether a busy victim caused queueing
- idle duration of each victim
- eviction cost of each victim

The swapper may cache the last decision exactly as the matrix swapper does, because the scheduler calls `EvictionFor` and then `OnSwapStart` with the same target/running set. Cache validity must also include an activity-generation counter so an `OnServeDone` event invalidates stale LRU decisions.

## Concurrency and state ownership

The router scheduler runs on one event-loop goroutine. Keep activity-hook mutations on that goroutine.

`EvictionFor` should read activity state without starting goroutines or performing I/O.

If the swapper is only accessed by the scheduler event loop, no mutex is needed. Document this assumption next to the activity map.

Do not inspect process state directly inside the swapper. Continue using the running set supplied by the scheduler.

## Minimal shared-file changes

The target is to keep recurring upstream-conflict surfaces limited to:

1. Router selection/registration switch: add `budget`.
2. Configuration root/settings: add `BudgetConfig`.
3. FIFO scheduler: add optional lifecycle notifications at request start, request completion, and successful model readiness.
4. Config schema and example documentation.

Everything else should live in new files.

Where upstream introduces new factory abstractions, adapt the budget router to those abstractions instead of preserving copied construction code.

Avoid copying large upstream files into fork-specific variants.

## Tests

### Solver unit tests

Add table-driven tests for:

- target already running and budget satisfied
- target not running and budget satisfied
- one idle victim is sufficient
- multiple idle victims are required
- longest-idle victim wins
- idle-only plan wins over a plan containing a busy model
- busy model selected when no idle-only plan is sufficient
- lower eviction cost breaks an LRU tie
- fewer victims break a cost tie
- lower excess memory breaks a remaining tie
- deterministic model-ID tie-break
- target is never evicted
- duplicate running IDs are counted once
- exact-budget boundary
- single model larger than budget rejected during validation
- missing memory metadata rejected during validation

### Scheduler integration tests

Using fake effects and a fake clock:

- a request starts immediately when the target fits
- an idle victim is stopped before loading the target
- a request queues when its required victim is in flight
- `OnServeDone` causes the queued request to be retried
- a newly idle older model becomes the selected victim
- group and matrix router behavior is unchanged when their swappers do not implement activity hooks

### Race and regression tests

- run `go test ./...`
- run `go test -race ./...` where supported
- preserve all upstream tests unchanged
- add a benchmark for subset selection with representative candidate counts

## Implementation phases

### Phase 1: static budget and LRU

- New budget config.
- Read `metadata.projected_total_mib`.
- New budget router and swapper.
- Activity tracking through optional hooks.
- Exhaustive subset solver.
- Reuse FIFO waiting behavior.
- Unit and integration tests.

### Phase 2: operational hardening

- Status/debug endpoint fields for configured and accounted memory.
- Better diagnostics for invalid metadata.
- Metrics for evictions, queued-for-memory requests, and reloads.
- Optional per-model minimum residency time to avoid immediate churn.
- Optional cooldown/hysteresis.

### Phase 3: optional future work

Not required for the initial fork:

- runtime process memory measurement
- Metal/unified-memory measurement
- adaptive estimates based on observed high-water marks
- KV-cache-aware accounting
- separate CPU RAM and VRAM budgets
- priority classes or pinned models

## Anti-thrashing safeguards

LRU alone can oscillate under alternating requests. Keep these optional and disabled by default in phase 1:

```yaml
eviction:
  policy: lru
  minimum_residency_seconds: 0
  minimum_idle_seconds: 0
  hysteresis_mib: 0
```

Potential semantics:

- `minimum_residency_seconds`: avoid evicting a model immediately after load unless no other plan is possible.
- `minimum_idle_seconds`: prefer waiting briefly over evicting a model that just became idle.
- `hysteresis_mib`: when eviction is necessary, free slightly more than the strict minimum to reduce repeated swaps.

These must remain secondary to progress: configuration must not create a permanent deadlock.

## Upstream synchronization workflow

Recommended Git layout:

```text
origin    -> Schott-Software-Solutions/llama-swap-vrambudget
upstream  -> mostlygeek/llama-swap
```

Regular synchronization:

```bash
git fetch upstream
git checkout main
git merge upstream/main
```

Keep fork changes in small, focused commits such as:

```text
config: add budget router settings
router: add budget swapper and solver
scheduler: emit optional activity lifecycle hooks
tests: cover budget eviction and FIFO waiting
docs: document VRAM budget router
```

Avoid mixing formatting or unrelated upstream cleanup into fork commits. That keeps conflict resolution local and makes it possible to upstream the feature later as separate pull requests.

## Definition of done for the first release

The first usable release is complete when:

- `routing.router.use: budget` is supported.
- Every managed model has a validated static MiB estimate.
- Any combination fitting the effective budget can remain loaded.
- A target that would exceed the budget causes an LRU-based eviction plan.
- Idle victims are preferred over busy victims.
- Busy victims are never interrupted; the request waits and is retried after completion.
- Existing matrix and group behavior remains unchanged.
- All upstream tests and new fork tests pass.
- The fork can merge a current upstream `main` with only small conflicts in the documented integration points.
