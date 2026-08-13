---
name: use-testify
description: Write and review Go unit tests using github.com/stretchr/testify. Use when creating, modifying, or reviewing Go _test.go files; selecting assert versus require; improving assertion diagnostics; testing errors, collections, timestamps, concurrency, generated Ent code, test fixtures, examples, benchmarks, or TestMain.
---

# Use Testify

Write tests that fail at the first broken prerequisite, report useful diffs, and verify observable behaviour without defensive test code.

## Start Each Test Deliberately

Create test-local assertion objects when a test has several assertions:

```go
func TestConvertCommentTree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	node, err := convertCommentNode(root)
	require.NoError(err, "convert a valid comment tree")
	require.NotNil(node, "valid input produces a node")

	assert.Equal(int64(100), node.GetComment().GetReplyId(), "preserve root reply ID")
}
```

Use `require` for prerequisites: an unexpected error, a required non-nil value, a successful type assertion, or any condition whose failure would make later checks meaningless or unsafe. Use `assert` for independent properties so one test run exposes all mismatches. `require` calls `FailNow`; call it only from the test goroutine, never from a goroutine started by the test.

For an expected error, use `require.Error(err)` first, then use `assert.EqualError(err, want)` to check its message. Usually compare a returned error with the error constructed by the relevant `pkg/apierrors/errors.go` constructor, for example `assert.EqualError(err, apierrors.NewErrNotFound().Error())`; do not inspect an error's internal structure unless its structured representation is the public behaviour under test.

Pass `msgAndArgs` to explain a case or assertion region when the reason is not obvious, especially for many equality checks. Prefer a short semantic message over restating expected and actual values.

## Required Rules

1. Replace ad-hoc `if err != nil { t.Fatalf(...) }` with `require.NoError(err)`. When an error is expected, use `require.Error(err)` and `assert.EqualError` for its message.
2. Assert collection sizes with `assert.Len` or `require.Len`, not `assert.Equal(want, len(got))`; length assertions produce better diagnostics for the actual collection.
3. Use `msgAndArgs` (the optional final arguments accepted by Testify assertions) to identify the case or reason for long or repeated assertion sequences.
4. Avoid type assertions by avoiding unnecessary `any`. When one is genuinely required, use the two-value form, `require.True(ok)`, then assert on the typed value; do not branch defensively around it.
5. Use `assert.Nil`, `assert.NotNil`, `require.Nil`, and `require.NotNil` for nilness. Choose `require` when the next operation needs the value.
6. Do not assert panics casually. Test a panic only for an intentional, unacceptable condition such as a security invariant or impossible/out-of-range calculation; prefer `assert.PanicsWithError` or the appropriate `assert.Panics*` variant, never `require`.
7. Do not add `if`/`else` control flow inside a unit test as a fallback. Record branch execution with booleans or counters and assert the expected branch coverage after exercising the code.
8. Do not unit-test generated Ent schemas or generated Ent SQL. Test handwritten behaviour instead. Test handwritten SQL or a SQL builder only when explicitly required and document why SQL text itself is the contract.
9. For repeated CRUD fixtures, provide composable, test-only `CreateTestXxx` helpers in the owning model package. Gate them with Go build constraints and place them in files ending in `_tutil.go`; do not repeat ad-hoc setup and mocks in each test.
10. Do not routinely mock `time.Now`. Keep business logic independent of clock checks where possible. Assert time windows with `assert.InRange`; otherwise compare Unix timestamps with `assert.GreaterOrEqual` or `assert.LessOrEqual`.
11. Do not assert incidental IDs. Generate inputs with `github.com/nekomeowww/xo` random helpers, `uuid.New()`, or similarly random-proof strings, then assert the behavioural relationship that matters rather than a fixed identifier.
12. Put materially different cases for one behaviour in `t.Run("UpperCamelCaseCaseName", func(t *testing.T) { ... })` subtests.
13. Use an extra `{ ... }` block to separate unrelated assertion regions in a long test.
14. Do not make tests defensive or forgiving. Avoid fallbacks such as `lo.FromPtr`, `lo.Coalesce`, and guard helpers that bypass a failing precondition. Let the test expose the boundary failure so production code can be fixed.
15. Treat floating-point and complex assertions carefully. Prefer `assert.InDelta` for floating-point values.
16. Avoid ad-hoc functions and helpers in `_test.go`. A need for data conversion, extraction, or reusable setup usually signals a missing model, service, utility, or test-only model helper. The normal exceptions are interface implementations and receiver methods required by the test.
17. Put runnable examples and benchmarks in `_test.go` files.
18. Make unit tests safe for `t.Parallel()` whenever isolation permits. Package-level mutable state and locks are design smells except in deliberately concurrent utilities.
19. Use `TestMain` only to initialize genuinely necessary shared resources (for example, test-only OpenTelemetry). Its exit form must be exactly:

    ```go
    func TestMain(m *testing.M) {
	    os.Exit(m.Run())
    }
    ```

20. Do not unit-test logs, stdio, stdout, or other standard-output side effects.
21. Use the testing utilities supplied by the framework or project—such as Kubernetes, Go Git, Fx, Dig, Gin, or Echo utilities—after consulting their Go documentation. Do not reinvent them.
22. For code that starts goroutines, uses channels, or reads/writes maps or slices concurrently, write a controlled concurrency test: use `t.Parallel()` when isolation permits and coordinate completion with `sync.WaitGroup` (and channels where appropriate). Never launch work and leave its completion unobserved.
23. Do not assert `fmt.Sprintf("%T", value)` or `fmt.Sprint(value)` output. Assert the actual type or behaviour instead.

## Review Checklist

- Use `require` for all prerequisite errors and values needed by later assertions.
- Keep assertions direct, typed, and diagnostic: `Len`, `Nil`, `InDelta`, `InRange`, and `EqualError` where they express the contract.
- Make test data random-proof and reusable without hiding the behaviour under test.
- Keep tests parallel-safe, deterministic, and free of generated-code, log-output, or incidental-representation assertions.
