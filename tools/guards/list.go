package main

// guards lists every guard of the repository with the mutation that disables it and the
// test that must then fail. A session that adds a guard adds it here (CLAUDE.md, rule 13).
var guards = []Guard{
	// Session 1: the timing budget of architecture §13. The worker refuses to start when a
	// relation does not hold.
	{
		Name: "config: dial timeout < send timeout", File: "internal/config/config.go",
		Old: "if w.DialTimeout >= w.SendTimeout {", New: "if false && w.DialTimeout >= w.SendTimeout {",
		Package: "./internal/config", Test: "TestDialTimeoutMustBeShorterThanSendTimeout",
	},
	{
		Name: "config: TLS handshake timeout < send timeout", File: "internal/config/config.go",
		Old: "if w.TLSHandshakeTimeout >= w.SendTimeout {", New: "if false && w.TLSHandshakeTimeout >= w.SendTimeout {",
		Package: "./internal/config", Test: "TestTLSHandshakeTimeoutMustBeShorterThanSendTimeout",
	},
	{
		Name: "config: gate budget < send timeout", File: "internal/config/config.go",
		Old: "if w.GateBudget >= w.SendTimeout {", New: "if false && w.GateBudget >= w.SendTimeout {",
		Package: "./internal/config", Test: "TestGateBudgetMustBeShorterThanSendTimeout",
	},
	{
		Name: "config: lease >= send timeout + 30 s", File: "internal/config/config.go",
		Old: "if w.Lease < w.SendTimeout+ResultWriteReserve {", New: "if false && w.Lease < w.SendTimeout+ResultWriteReserve {",
		Package: "./internal/config", Test: "TestLeaseMustCoverSendTimeoutPlusResultWrite",
	},
	{
		Name: "config: lease bound is inclusive", File: "internal/config/config.go",
		Old: "if w.Lease < w.SendTimeout+ResultWriteReserve {", New: "if w.Lease <= w.SendTimeout+ResultWriteReserve {",
		Package: "./internal/config", Test: "TestLeaseMustCoverSendTimeoutPlusResultWrite",
	},
	{
		Name: "config: breaker open > send timeout", File: "internal/config/config.go",
		Old: "if w.BreakerOpen <= w.SendTimeout {", New: "if false && w.BreakerOpen <= w.SendTimeout {",
		Package: "./internal/config", Test: "TestBreakerOpenTimeMustExceedSendTimeout",
	},
	{
		Name: "config: shutdown timeout >= send timeout + 5 s", File: "internal/config/config.go",
		Old: "if w.ShutdownTimeout < w.SendTimeout+ShutdownReserve {", New: "if false && w.ShutdownTimeout < w.SendTimeout+ShutdownReserve {",
		Package: "./internal/config", Test: "TestShutdownTimeoutMustCoverSendTimeoutPlusFiveSeconds",
	},
	{
		Name: "config: idle connections = slots", File: "internal/config/config.go",
		Old: "if w.IdleConns != w.Slots {", New: "if w.IdleConns < w.Slots {",
		Package: "./internal/config", Test: "TestIdleConnectionsMustEqualSlots",
	},
	{
		Name: "config: claim batch <= slots", File: "internal/config/config.go",
		Old: "if w.ClaimBatch > w.Slots {", New: "if false && w.ClaimBatch > w.Slots {",
		Package: "./internal/config", Test: "TestClaimBatchMustNotExceedSlots",
	},
	{
		Name: "config: every setting is positive", File: "internal/config/config.go",
		Old: "if s.value <= 0 {", New: "if false && s.value <= 0 {",
		Package: "./internal/config", Test: "TestNonPositiveSettingsAreRefused",
	},
	{
		Name: "worker: refuses to start on a broken relation", File: "cmd/worker/main.go",
		Old: "if err := budget.Check(); err != nil {", New: "if err := error(nil); err != nil {",
		Package: "./cmd/worker", Test: "TestWorkerRefusesToStartWithALeaseShorterThanSendTimeoutPlus30s",
	},
	{
		Name: "config: shutdown timeout < stop grace period", File: "internal/config/config.go",
		Old: "if w.StopGracePeriod <= w.ShutdownTimeout {", New: "if false && w.StopGracePeriod <= w.ShutdownTimeout {",
		Package: "./internal/config", Test: "TestShutdownTimeoutMustEndBeforeTheStopGracePeriod",
	},
	{
		Name: "compose: the worker is told its stop grace period", File: "docker-compose.yml",
		Old: "      STOP_GRACE_PERIOD: ${STOP_GRACE_PERIOD:-30s}\n", New: "",
		Package: "./internal/config", Test: "TestComposeGivesTheWorkerItsStopGracePeriod",
	},

	// Session 1: the lifecycle of architecture §3.
	{
		Name: "domain: the reaper may return a processing row", File: "internal/domain/domain.go",
		Old: "Processing: {Sent, Retrying, Failed, Pending},", New: "Processing: {Sent, Retrying, Failed},",
		Package: "./internal/domain", Test: "TestStatusTransitionsFollowTheLifecycle",
	},
	{
		Name: "domain: sent is final", File: "internal/domain/domain.go",
		Old: "Sent:       nil,", New: "Sent:       {Pending},",
		Package: "./internal/domain", Test: "TestOnlySentAndCancelledAreTerminal",
	},

	// Session 1: the schema of architecture §4, tied to package domain.
	{
		Name: "schema: claim index holds every claimable status", File: "internal/store/migrations/00002_notifications.sql",
		Old: "WHERE status IN ('scheduled', 'pending', 'retrying');", New: "WHERE status IN ('scheduled', 'pending');",
		Package: "./internal/store", Test: "TestClaimIndexCoversExactlyTheClaimableStatuses",
	},
	{
		Name: "schema: status check refuses unknown statuses", File: "internal/store/migrations/00002_notifications.sql",
		Old: "'sent', 'failed', 'cancelled')),", New: "'sent', 'failed', 'cancelled', 'done')),",
		Package: "./internal/store", Test: "TestColumnChecksAcceptExactlyTheDomainValues",
	},
	{
		Name: "schema: status check matches domain exactly", File: "internal/store/migrations/00002_notifications.sql",
		Old: "'sent', 'failed', 'cancelled')),", New: "'sent', 'failed', 'cancelled', 'archived')),",
		Package: "./internal/store", Test: "TestColumnChecksAllowExactlyTheDomainValues",
	},
	{
		Name: "schema: channel check matches domain exactly", File: "internal/store/migrations/00002_notifications.sql",
		Old: "NOT NULL CHECK (channel IN ('sms', 'email', 'push')),", New: "NOT NULL CHECK (channel IN ('sms', 'email', 'push', 'pager')),",
		Package: "./internal/store", Test: "TestColumnChecksAllowExactlyTheDomainValues",
	},
	{
		Name: "schema: priority check matches domain exactly", File: "internal/store/migrations/00002_notifications.sql",
		Old: "CHECK (priority IN (0, 1, 2))", New: "CHECK (priority IN (0, 1, 2, 3))",
		Package: "./internal/store", Test: "TestColumnChecksAllowExactlyTheDomainValues",
	},
	{
		Name: "schema: template channel check matches domain exactly", File: "internal/store/migrations/00004_templates.sql",
		Old: "NOT NULL CHECK (channel IN ('sms', 'email', 'push')),", New: "NOT NULL CHECK (channel IN ('sms', 'email', 'push', 'pager')),",
		Package: "./internal/store", Test: "TestColumnChecksAllowExactlyTheDomainValues",
	},
	{
		Name: "schema: batch idempotency key is unique", File: "internal/store/migrations/00001_batches.sql",
		Old: "idempotency_key text        UNIQUE,", New: "idempotency_key text,",
		Package: "./internal/store", Test: "TestIdempotencyKeysAreUniqueInTheDatabase",
	},
	{
		Name: "schema: an attempt belongs to a notification", File: "internal/store/migrations/00003_delivery_attempts.sql",
		Old: "NOT NULL REFERENCES notifications (id),", New: "NOT NULL,",
		Package: "./internal/store", Test: "TestAnAttemptNumberIsRecordedOnce",
	},
	{
		Name: "schema: a notification belongs to a known batch", File: "internal/store/migrations/00002_notifications.sql",
		Old: "uuid        REFERENCES batches (id),", New: "uuid,",
		Package: "./internal/store", Test: "TestAnAttemptNumberIsRecordedOnce",
	},
	{
		Name: "schema: reap index holds processing rows", File: "internal/store/migrations/00002_notifications.sql",
		Old: "WHERE status = 'processing';", New: "WHERE status <> 'sent';",
		Package: "./internal/store", Test: "TestReapListAndBatchIndexesExist",
	},
	{
		Name: "store: sessions end idle transactions", File: "internal/store/store.go",
		Old: `params["idle_in_transaction_session_timeout"] =`, New: `_ =`,
		Package: "./internal/store", Test: "TestServiceSessionsCarryTheirTimeouts",
	},
	{
		Name: "store: sessions carry the statement timeout", File: "internal/store/store.go",
		Old: `params["statement_timeout"] =`, New: `_ =`,
		Package: "./internal/store", Test: "TestServiceSessionsCarryTheirTimeouts",
	},
	{
		Name: "schema: notification idempotency key is unique", File: "internal/store/migrations/00002_notifications.sql",
		Old: "idempotency_key     text        UNIQUE,", New: "idempotency_key     text,",
		Package: "./internal/store", Test: "TestIdempotencyKeysAreUniqueInTheDatabase",
	},
	{
		Name: "schema: one row per attempt number", File: "internal/store/migrations/00003_delivery_attempts.sql",
		Old: "PRIMARY KEY (notification_id, attempt_number)", New: "PRIMARY KEY (notification_id, attempt_number, started_at)",
		Package: "./internal/store", Test: "TestAnAttemptNumberIsRecordedOnce",
	},
	{
		Name: "schema: migrations are append-only", File: "internal/store/migrations/00004_templates.sql",
		Old: "CREATE TABLE templates (", New: "CREATE TABLE templates ( -- edited after it was recorded",
		Package: "./internal/store", Test: "TestMigrationsAreAppendOnly",
	},

	// Session 1: logging and the correlation id (architecture §8, §10).
	{
		Name: "logs: correlation id stays at the top level", File: "internal/observability/logging.go",
		Old:     "inner = inner.WithAttrs([]slog.Attr{slog.String(\"correlation_id\", id)})",
		New:     "r.AddAttrs(slog.String(\"correlation_id\", id))",
		Package: "./internal/observability", Test: "TestCorrelationIDStaysTopLevelUnderGroups",
	},
	{
		Name: "logs: correlation id length bound", File: "internal/observability/logging.go",
		Old: "len(s) > MaxCorrelationIDLength", New: "len(s) > 1024",
		Package: "./internal/observability", Test: "TestCorrelationIDBound",
	},
	{
		Name: "logs: correlation id characters", File: "internal/observability/logging.go",
		Old: "if !ok {\n\t\t\treturn false", New: "if false && !ok {\n\t\t\treturn false",
		Package: "./internal/observability", Test: "TestCorrelationIDBound",
	},
	{
		Name: "api: a client id over the bound is replaced", File: "internal/api/health.go",
		Old: "if !observability.ValidCorrelationID(id) {", New: "if id == \"\" {",
		Package: "./internal/api", Test: "TestCorrelationIDIsAcceptedOrReplaced",
	},

	// Session 1: health endpoints and request bounds (architecture §8).
	{
		Name: "api: readyz checks Postgres", File: "internal/api/health.go",
		Old:     `checks := map[string]Check{"postgres": h.Postgres, "redis": h.Redis}`,
		New:     `checks := map[string]Check{"postgres": func(context.Context) error { return nil }, "redis": h.Redis}`,
		Package: "./internal/api", Test: "TestReadyzFailsWhenPostgresIsUnreachable",
	},
	{
		Name: "api: readyz reports Redis", File: "internal/api/health.go",
		Old:     `checks := map[string]Check{"postgres": h.Postgres, "redis": h.Redis}`,
		New:     `checks := map[string]Check{"postgres": h.Postgres, "redis": func(context.Context) error { return nil }}`,
		Package: "./internal/api", Test: "TestReadyzStaysReadyWhenOnlyRedisIsUnreachable",
	},
	{
		Name: "api: only Postgres decides readiness", File: "internal/api/health.go",
		Old: `if result["postgres"] != "ok" {`, New: `if result["postgres"] != "ok" || result["redis"] != "ok" {`,
		Package: "./internal/api", Test: "TestReadyzStaysReadyWhenOnlyRedisIsUnreachable",
	},
	{
		Name: "api: database sessions have a 5 s statement timeout", File: "internal/api/health.go",
		Old: "var DatabaseOptions = store.Options{StatementTimeout: 5 * time.Second}", New: "var DatabaseOptions = store.Options{}",
		Package: "./internal/api", Test: "TestAPIDatabaseSessionsHaveAFiveSecondStatementTimeout",
	},
	{
		Name: "redis: clients honour context deadlines", File: "internal/redisclient/redisclient.go",
		Old: "opts.ContextTimeoutEnabled = true", New: "opts.ContextTimeoutEnabled = false",
		Package: "./internal/redisclient", Test: "TestRedisClientHonoursContextDeadlines",
	},
	{
		Name: "redis: readyz with real clients is bounded", File: "internal/redisclient/redisclient.go",
		Old: "opts.ContextTimeoutEnabled = true", New: "opts.ContextTimeoutEnabled = false",
		Package: "./internal/api", Test: "TestReadyzAnswersWithinItsTimeoutWhenRealClientsHang",
	},
	{
		Name: "api: readyz checks are bounded in time", File: "internal/api/health.go",
		Old: "ctx, cancel := context.WithTimeout(r.Context(), h.Timeout)", New: "ctx, cancel := context.WithCancel(r.Context())",
		Package: "./internal/api", Test: "TestReadyzAnswersWithinItsTimeoutWhenADependencyHangs",
	},
	{
		Name: "api: readyz hides dependency errors", File: "internal/api/health.go",
		Old: `state = "unreachable"`, New: `state = err.Error()`,
		Package: "./internal/api", Test: "TestReadyzDoesNotShowDependencyErrors",
	},
	{
		Name: "api: request header size bound", File: "internal/api/health.go",
		Old: "MaxHeaderBytes:    MaxHeaderBytes - headerSlack,", New: "MaxHeaderBytes:    MaxHeaderBytes,",
		Package: "./internal/api", Test: "TestRequestHeadersOverTheBoundAreRefused",
	},
	{
		Name: "api: request header time bound", File: "internal/api/health.go",
		Old: "ReadHeaderTimeout: headerTimeout,", New: "ReadHeaderTimeout: 0,",
		Package: "./internal/api", Test: "TestSlowRequestHeadersAreCutOff",
	},

	// Session 1: the recording provider server. Guarantees are measured here, so its own
	// guards are guards of every later delivery test.
	{
		Name: "recorder: stamped before the body is read", File: "internal/recorder/recorder.go",
		Old:     "body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, rec.opts.MaxBody))",
		New:     "body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, rec.opts.MaxBody))\n\tarrived = time.Now()",
		Package: "./internal/recorder", Test: "TestRecorderStampsBeforeTheBodyIsRead",
	},
	{
		Name: "recorder: stamped before the lock", File: "internal/recorder/recorder.go",
		Old: "\tarrived := time.Now()\n\trec.mu.Lock()\n", New: "\trec.mu.Lock()\n\tarrived := time.Now()\n",
		Package: "./internal/recorder", Test: "TestRecorderStampIsNotDelayedByReaders",
	},
	{
		Name: "recorder: injected delay", File: "internal/recorder/recorder.go",
		Old: "if resp.Delay > 0 {", New: "if false && resp.Delay > 0 {",
		Package: "./internal/recorder", Test: "TestRecorderStampsOnArrivalNotOnResponse",
	},
	{
		Name: "recorder: closed connection without a response", File: "internal/recorder/recorder.go",
		Old: "if resp.Close {", New: "if false && resp.Close {",
		Package: "./internal/recorder", Test: "TestRecorderClosesTheConnectionWithoutAResponse",
	},
	{
		Name: "recorder: slow TLS handshake", File: "internal/recorder/server.go",
		Old: "if opts.HandshakeDelay > 0 {\n\t\tts.Listener =", New: "if false {\n\t\tts.Listener =",
		Package: "./internal/recorder", Test: "TestRecorderDelaysTheTLSHandshake",
	},
	{
		Name: "recorder: request body bound", File: "internal/recorder/recorder.go",
		Old: "io.ReadAll(http.MaxBytesReader(w, r.Body, rec.opts.MaxBody))", New: "io.ReadAll(r.Body)",
		Package: "./internal/recorder", Test: "TestRecorderRefusesABodyOverItsBound",
	},
	{
		Name: "recorder: windows are half-open", File: "internal/recorder/recorder.go",
		Old: "ts[j].Before(ts[i].Add(window))", New: "!ts[j].After(ts[i].Add(window))",
		Package: "./internal/recorder", Test: "TestMaxInWindowCountsHalfOpenWindows",
	},
	{
		Name: "recorder: slow setup only over TLS", File: "internal/recorder/server.go",
		Old: "if opts.HandshakeDelay > 0 && !opts.TLS {", New: "if false {",
		Package: "./internal/recorder", Test: "TestRecorderRefusesHandshakeDelayWithoutTLS",
	},

	// Session 1: the check that every test named in the documents exists.
	{
		Name: "testnames: a missing test is reported", File: "tools/testnames/main.go",
		Old: "if !tests[name] {", New: "if false && !tests[name] {",
		Package: "./tools/testnames", Test: "TestTestNamesCheckReportsATestThatDoesNotExist",
	},

	// Session 2: the queue of architecture §5.
	{
		Name: "queue: claims lock the rows they take (I2)", File: "internal/store/queue.go",
		Old: "    LIMIT $3\n    FOR UPDATE SKIP LOCKED))", New: "    LIMIT $3))",
		Package: "./internal/store", Test: "TestConcurrentClaimsNeverTakeTheSameRow",
	},
	{
		Name: "queue: a claim takes only waiting rows (I2)", File: "internal/store/queue.go",
		Old: "      AND status IN ('scheduled', 'pending', 'retrying')\n      AND ready_at <= now()", New: "      AND ready_at <= now()",
		Package: "./internal/store", Test: "TestClaimTakesExactlyTheClaimableStatuses",
	},
	{
		Name: "queue: a claim takes only due rows", File: "internal/store/queue.go",
		Old: "      AND ready_at <= now()\n", New: "",
		Package: "./internal/store", Test: "TestClaimReturnsOnlyDueRows",
	},
	{
		Name: "queue: a claim takes only its channel", File: "internal/store/queue.go",
		Old: "WHERE channel = $1 AND priority = $2", New: "WHERE $1::text <> '' AND priority = $2",
		Package: "./internal/store", Test: "TestClaimTakesOnlyItsChannelAndPriority",
	},
	{
		Name: "queue: a claim takes only its priority", File: "internal/store/queue.go",
		Old: "WHERE channel = $1 AND priority = $2", New: "WHERE channel = $1 AND $2::smallint >= 0",
		Package: "./internal/store", Test: "TestClaimTakesOnlyItsChannelAndPriority",
	},
	{
		Name: "queue: a claim stops after n rows", File: "internal/store/queue.go",
		Old: "    LIMIT $3\n", New: "    LIMIT $3 + 1\n",
		Package: "./internal/store", Test: "TestClaimReturnsTheEarliestRowsInReadyAtOrder",
	},
	{
		Name: "queue: the claim's order is in its SQL", File: "internal/store/queue.go",
		Old: "    ORDER BY ready_at\n    LIMIT $3", New: "    LIMIT $3",
		Package: "./internal/store", Test: "TestClaimTakesTheEarliestDueRowsWithoutTheIndex",
	},
	{
		Name: "queue: claimed rows come back in ready_at order", File: "internal/store/queue.go",
		Old: "slices.SortStableFunc(claimed, func(a, b Claimed) int { return a.ReadyAt.Compare(b.ReadyAt) })", New: "slices.SortStableFunc(claimed, func(a, b Claimed) int { return 0 })",
		Package: "./internal/store", Test: "TestClaimReturnsTheEarliestRowsInReadyAtOrder",
	},
	{
		Name: "queue: a claim is one attempt (I7)", File: "internal/store/queue.go",
		Old: "attempts = attempts + 1,", New: "attempts = attempts,",
		Package: "./internal/store", Test: "TestAttemptsNeverGoDown",
	},
	{
		Name: "queue: the lease is on the database clock (I12)", File: "internal/store/queue.go",
		Old: "locked_until = now() + $4::bigint * interval '1 microsecond'", New: "locked_until = clock_timestamp() + $4::bigint * interval '1 microsecond'",
		Package: "./internal/store", Test: "TestClaimSetsTheLeaseOnTheDatabaseClock",
	},
	{
		Name: "queue: a claim refuses an empty lease", File: "internal/store/queue.go",
		Old: "if lease <= 0 {", New: "if lease < 0 {",
		Package: "./internal/store", Test: "TestClaimRefusesAnEmptyLeaseAndAsksNothingForZeroRows",
	},
	{
		Name: "queue: A3, the index order alone serves the claim", File: "internal/store/queue.go",
		Old: "    ORDER BY ready_at\n", New: "    ORDER BY ready_at, id\n",
		Package: "./internal/store", Test: "TestClaimPlanReadsTheIndexInOrder",
	},
	{
		Name: "queue: A3, the ANY(ARRAY(...)) form", File: "internal/store/queue.go",
		Old: "WHERE id = ANY(ARRAY(\n    SELECT id FROM notifications\n    WHERE channel = $1 AND priority = $2\n      AND status IN ('scheduled', 'pending', 'retrying')\n      AND ready_at <= now()\n    ORDER BY ready_at\n    LIMIT $3\n    FOR UPDATE SKIP LOCKED))\nRETURNING id, recipient", New: "WHERE id IN (\n    SELECT id FROM notifications\n    WHERE channel = $1 AND priority = $2\n      AND status IN ('scheduled', 'pending', 'retrying')\n      AND ready_at <= now()\n    ORDER BY ready_at\n    LIMIT $3\n    FOR UPDATE SKIP LOCKED)\nRETURNING id, recipient",
		Package: "./internal/store", Test: "TestClaimPlanReadsTheIndexInOrder",
	},
	{
		Name: "queue: finish only from the current attempt (I3)", File: "internal/store/queue.go",
		Old: "WHERE id = $1 AND attempts = $2 AND status = 'processing'", New: "WHERE id = $1 AND $2::int > 0 AND status = 'processing'",
		Package: "./internal/store", Test: "TestFinishFromAStaleAttemptChangesNothing",
	},
	{
		Name: "queue: finish only while processing (I3)", File: "internal/store/queue.go",
		Old: "WHERE id = $1 AND attempts = $2 AND status = 'processing'", New: "WHERE id = $1 AND attempts = $2",
		Package: "./internal/store", Test: "TestFinishOnARowThatIsNoLongerProcessingChangesNothing",
	},
	{
		Name: "queue: a repeated finish is harmless (I3)", File: "internal/store/queue.go",
		Old: "ON CONFLICT DO NOTHING", New: "",
		Package: "./internal/store", Test: "TestRepeatedFinishAddsNoSecondAttemptRow",
	},
	{
		Name: "queue: a stale result is kept as history", File: "internal/store/queue.go",
		Old: "return nil // stale", New: "return err // stale",
		Package: "./internal/store", Test: "TestFinishFromAStaleAttemptChangesNothing",
	},
	{
		Name: "queue: finish stores only results", File: "internal/store/queue.go",
		Old: "case r.Status != domain.Sent && r.Status != domain.Failed && r.Status != domain.Retrying:", New: "case false && r.Status != domain.Sent && r.Status != domain.Failed && r.Status != domain.Retrying:",
		Package: "./internal/store", Test: "TestFinishRefusesAStatusThatIsNotAResult",
	},
	{
		Name: "queue: a retry on the last attempt is failed", File: "internal/store/queue.go",
		Old: "WHEN $3::text = 'retrying' AND attempts >= max_attempts THEN 'failed'", New: "WHEN false THEN 'failed'",
		Package: "./internal/store", Test: "TestARetryOnTheLastAttemptIsStoredAsFailed",
	},
	{
		Name: "queue: attempt start time on the database clock (I12)", File: "internal/store/queue.go",
		Old: "now() - $3::int * interval '1 millisecond'", New: "clock_timestamp() - $3::int * interval '1 millisecond'",
		Package: "./internal/store", Test: "TestFinishStoresEachResultOnTheDatabaseClock",
	},
	{
		Name: "queue: stored text has no NUL", File: "internal/store/queue.go",
		Old: "strings.ReplaceAll(strings.ToValidUTF8(s, string(utf8.RuneError)), \"\\x00\", \"\")", New: "strings.ToValidUTF8(s, string(utf8.RuneError))",
		Package: "./internal/store", Test: "TestFinishStoresAnyErrorTextWithinItsBound",
	},
	{
		Name: "queue: stored text is bounded", File: "internal/store/queue.go",
		Old: "if len(s) <= limit {", New: "if true || len(s) <= limit {",
		Package: "./internal/store", Test: "TestFinishStoresTextAtItsBound",
	},
	{
		Name: "queue: reap takes only ended leases (I4)", File: "internal/store/queue.go",
		Old: "WHERE status = 'processing' AND locked_until < now()", New: "WHERE status = 'processing'",
		Package: "./internal/store", Test: "TestReapReturnsExpiredRowsToPendingOrFailed",
	},
	{
		Name: "queue: reap fails a row at its last attempt (I4)", File: "internal/store/queue.go",
		Old: "SET status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END", New: "SET status = 'pending'",
		Package: "./internal/store", Test: "TestReapReturnsExpiredRowsToPendingOrFailed",
	},
	{
		Name: "queue: reap takes a bounded batch", File: "internal/store/queue.go",
		Old: "    LIMIT $1\n", New: "    LIMIT $1 + 5\n",
		Package: "./internal/store", Test: "TestReapReturnsExpiredRowsToPendingOrFailed",
	},
	{
		Name: "queue: reapers lock the rows they take (I4)", File: "internal/store/queue.go",
		Old: "    LIMIT $1\n    FOR UPDATE SKIP LOCKED))", New: "    LIMIT $1))",
		Package: "./internal/store", Test: "TestTwoReapersAtOnceReturnEachRowOnce",
	},
	{
		Name: "queue: cancel only from waiting statuses", File: "internal/store/queue.go",
		Old: "WHERE id = $1 AND status IN ('scheduled', 'pending', 'retrying')", New: "WHERE id = $1",
		Package: "./internal/store", Test: "TestCancelOnlyFromWaitingStatuses",
	},
	{
		Name: "queue: a missing row is not a wrong status", File: "internal/store/queue.go",
		Old: "fmt.Errorf(\"%s %s: %w\", op, id, ErrNotFound)", New: "fmt.Errorf(\"%s %s: %w\", op, id, ErrWrongStatus)",
		Package: "./internal/store", Test: "TestCancelOnlyFromWaitingStatuses",
	},
	{
		Name: "queue: requeue only from failed", File: "internal/store/queue.go",
		Old: "WHERE id = $1 AND status = 'failed'", New: "WHERE id = $1",
		Package: "./internal/store", Test: "TestRequeueRaisesMaxAttemptsByTheBudget",
	},
	{
		Name: "queue: requeue counts the budget from attempts (I7)", File: "internal/store/queue.go",
		Old: "max_attempts = attempts + $2", New: "max_attempts = $2::int",
		Package: "./internal/store", Test: "TestRequeueRaisesMaxAttemptsByTheBudget",
	},
	{
		Name: "queue: writes ignore the caller's cancellation", File: "internal/store/queue.go",
		Old: "context.WithoutCancel(ctx)", New: "ctx",
		Package: "./internal/store", Test: "TestQueueWritesAreNotCutOffByACancelledCaller",
	},
	{
		Name: "queue: writes have their own timeout", File: "internal/store/queue.go",
		Old: "context.WithTimeout(context.WithoutCancel(ctx), q.timeout)", New: "context.WithCancel(context.WithoutCancel(ctx))",
		Package: "./internal/store", Test: "TestQueueWritesHaveTheirOwnTimeout",
	},

	// Session 2, after the review: the skip half of SKIP LOCKED, and the text bounds at
	// their edge.
	{
		Name: "queue: claims skip locked rows instead of waiting", File: "internal/store/queue.go",
		Old: "    LIMIT $3\n    FOR UPDATE SKIP LOCKED))", New: "    LIMIT $3\n    FOR UPDATE))",
		Package: "./internal/store", Test: "TestClaimSkipsRowsLockedByAnotherTransaction",
	},
	{
		Name: "queue: reapers skip locked rows instead of waiting", File: "internal/store/queue.go",
		Old: "    LIMIT $1\n    FOR UPDATE SKIP LOCKED))", New: "    LIMIT $1\n    FOR UPDATE))",
		Package: "./internal/store", Test: "TestReapSkipsRowsLockedByAnotherTransaction",
	},
	{
		Name: "queue: the text bound is inclusive", File: "internal/store/queue.go",
		Old: "if len(s) <= limit {", New: "if len(s) < limit {",
		Package: "./internal/store", Test: "TestFinishStoresTextAtItsBound",
	},
	{
		Name: "queue: text is cut at a character boundary", File: "internal/store/queue.go",
		Old: "for cut > 0 && !utf8.RuneStart(s[cut]) {", New: "for false && cut > 0 && !utf8.RuneStart(s[cut]) {",
		Package: "./internal/store", Test: "TestFinishStoresTextAtItsBound",
	},
	{
		Name: "queue: the outcome text is bounded", File: "internal/store/queue.go",
		Old: "bounded(r.Outcome, MaxErrorBytes)", New: "r.Outcome",
		Package: "./internal/store", Test: "TestFinishStoresTextAtItsBound",
	},

	// Session 3: the rate limiter of architecture §7.
	{
		Name: "ratelimit: at most the limit per window (A6)", File: "internal/ratelimit/ratelimit.go",
		Old: "if redis.call('ZCARD', key) < limit then", New: "if redis.call('ZCARD', key) <= limit then",
		Package: "./internal/ratelimit", Test: "TestNoWindowHoldsMoreThanTheLimit",
	},
	{
		Name: "ratelimit: a permit is refused at the limit", File: "internal/ratelimit/ratelimit.go",
		Old: "if redis.call('ZCARD', key) < limit then", New: "if redis.call('ZCARD', key) <= limit then",
		Package: "./internal/ratelimit", Test: "TestAPermitIsRefusedAtTheLimit",
	},
	{
		Name: "ratelimit: a permit counts for the whole window", File: "internal/ratelimit/ratelimit.go",
		Old: "now - window))\n", New: "now - window + 100000))\n",
		Package: "./internal/ratelimit", Test: "TestNoWindowHoldsMoreThanTheLimit",
	},
	{
		Name: "ratelimit: permits past the window are removed", File: "internal/ratelimit/ratelimit.go",
		Old: "redis.call('ZREMRANGEBYSCORE', key, '-inf', string.format('%d', now - window))\n", New: "",
		Package: "./internal/ratelimit", Test: "TestWaitReturnsWhenTheOldestPermitLeavesTheWindow",
	},
	{
		Name: "ratelimit: the window is one second plus the margin", File: "internal/ratelimit/ratelimit.go",
		Old: "window: time.Second + margin}", New: "window: time.Second}",
		Package: "./internal/ratelimit", Test: "TestAPermitCountsForOneSecondPlusTheMargin",
	},
	{
		Name: "ratelimit: a script error is not a permit", File: "internal/ratelimit/ratelimit.go",
		Old: "return 0, err // fail closed", New: "return 0, nil // fail closed",
		Package: "./internal/ratelimit", Test: "TestAvailableWorksAndWaitFailsWhenOnlyTheScriptFails",
	},
	{
		Name: "ratelimit: an unreachable Redis is not a permit", File: "internal/ratelimit/ratelimit.go",
		Old: "return 0, err // fail closed", New: "return 0, nil // fail closed",
		Package: "./internal/ratelimit", Test: "TestAnUnreachableRedisIsAnErrorNotAPermit",
	},
	{
		Name: "ratelimit: a limiter error is not ErrNoPermitInTime", File: "internal/ratelimit/ratelimit.go",
		Old: "return 0, err // fail closed", New: "return 0, fmt.Errorf(\"%w: %w\", ErrNoPermitInTime, err) // fail closed",
		Package: "./internal/ratelimit", Test: "TestAHungRedisIsALimiterErrorNotAWaitThatRanOut",
	},
	{
		Name: "ratelimit: Wait stops when its context ends", File: "internal/ratelimit/ratelimit.go",
		Old: "\t\tcase <-ctx.Done():\n\t\t\ttimer.Stop()\n\t\t\treturn 0, fmt.Errorf(\"%w (%w)\", ErrNoPermitInTime, ctx.Err())\n", New: "",
		Package: "./internal/ratelimit", Test: "TestWaitStopsWhenItsContextEnds",
	},
	{
		Name: "ratelimit: an ended context is ErrNoPermitInTime", File: "internal/ratelimit/ratelimit.go",
		Old: "if err := ctx.Err(); err != nil {", New: "if err := ctx.Err(); false && err != nil {",
		Package: "./internal/ratelimit", Test: "TestWaitWithAnEndedContextTakesNoPermit",
	},
	{
		Name: "ratelimit: Available only reads", File: "internal/ratelimit/ratelimit.go",
		Old: "#!lua flags=no-writes\nlocal held = redis.call('GET', KEYS[2])\nif held and held ~= ARGV[1] .. '/' .. ARGV[2] then\n  return redis.error_reply('rate limit: this worker has limit/window ' .. ARGV[1] .. '/' .. ARGV[2] .. ', the window has ' .. held)\nend\nlocal t = redis.call('TIME')\nlocal now = tonumber(t[1]) * 1000000 + tonumber(t[2])\nlocal used = redis.call('ZCOUNT', KEYS[1], string.format('(%d', now - tonumber(ARGV[2])), '+inf')\nreturn math.max(0, tonumber(ARGV[1]) - used)\n`)\n\n// Available returns the number of free permits of the channel now. It changes nothing; the\n// answer is a hint that can be wrong a moment later (architecture §6). An unreachable Redis is\n// an error.\nfunc (l *Limiter) Available(ctx context.Context, ch domain.Channel) (int, error) {\n\tn, err := readScript.RunRO(", New: "#!lua\nlocal held = redis.call('GET', KEYS[2])\nif held and held ~= ARGV[1] .. '/' .. ARGV[2] then\n  return redis.error_reply('rate limit: this worker has limit/window ' .. ARGV[1] .. '/' .. ARGV[2] .. ', the window has ' .. held)\nend\nlocal t = redis.call('TIME')\nlocal now = tonumber(t[1]) * 1000000 + tonumber(t[2])\nredis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', string.format('%d', now - tonumber(ARGV[2])))\nlocal used = redis.call('ZCARD', KEYS[1])\nreturn math.max(0, tonumber(ARGV[1]) - used)\n`)\n\n// Available returns the number of free permits of the channel now. It changes nothing; the\n// answer is a hint that can be wrong a moment later (architecture §6). An unreachable Redis is\n// an error.\nfunc (l *Limiter) Available(ctx context.Context, ch domain.Channel) (int, error) {\n\tn, err := readScript.Run(",
		Package: "./internal/ratelimit", Test: "TestAvailableChangesNothing",
	},
	{
		Name: "ratelimit: Available works when only writes fail", File: "internal/ratelimit/ratelimit.go",
		Old: "#!lua flags=no-writes\nlocal held = redis.call('GET', KEYS[2])\nif held and held ~= ARGV[1] .. '/' .. ARGV[2] then\n  return redis.error_reply('rate limit: this worker has limit/window ' .. ARGV[1] .. '/' .. ARGV[2] .. ', the window has ' .. held)\nend\nlocal t = redis.call('TIME')\nlocal now = tonumber(t[1]) * 1000000 + tonumber(t[2])\nlocal used = redis.call('ZCOUNT', KEYS[1], string.format('(%d', now - tonumber(ARGV[2])), '+inf')\nreturn math.max(0, tonumber(ARGV[1]) - used)\n`)\n\n// Available returns the number of free permits of the channel now. It changes nothing; the\n// answer is a hint that can be wrong a moment later (architecture §6). An unreachable Redis is\n// an error.\nfunc (l *Limiter) Available(ctx context.Context, ch domain.Channel) (int, error) {\n\tn, err := readScript.RunRO(", New: "#!lua\nlocal held = redis.call('GET', KEYS[2])\nif held and held ~= ARGV[1] .. '/' .. ARGV[2] then\n  return redis.error_reply('rate limit: this worker has limit/window ' .. ARGV[1] .. '/' .. ARGV[2] .. ', the window has ' .. held)\nend\nlocal t = redis.call('TIME')\nlocal now = tonumber(t[1]) * 1000000 + tonumber(t[2])\nredis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', string.format('%d', now - tonumber(ARGV[2])))\nlocal used = redis.call('ZCARD', KEYS[1])\nreturn math.max(0, tonumber(ARGV[1]) - used)\n`)\n\n// Available returns the number of free permits of the channel now. It changes nothing; the\n// answer is a hint that can be wrong a moment later (architecture §6). An unreachable Redis is\n// an error.\nfunc (l *Limiter) Available(ctx context.Context, ch domain.Channel) (int, error) {\n\tn, err := readScript.Run(",
		Package: "./internal/ratelimit", Test: "TestAvailableWorksAndWaitFailsWhenOnlyTheScriptFails",
	},
	{
		Name: "ratelimit: a refused call adds nothing", File: "internal/ratelimit/ratelimit.go",
		Old: "local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')", New: "redis.call('ZADD', key, string.format('%d', now), ARGV[3])\nlocal oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')",
		Package: "./internal/ratelimit", Test: "TestTheWindowHoldsAtMostTheLimitAndExpires",
	},
	{
		Name: "ratelimit: the window expires after its last permit", File: "internal/ratelimit/ratelimit.go",
		Old: "  redis.call('PEXPIRE', key, math.floor(window / 1000) + 1)\n", New: "",
		Package: "./internal/ratelimit", Test: "TestTheWindowHoldsAtMostTheLimitAndExpires",
	},
	{
		Name: "ratelimit: the settings expire with the window", File: "internal/ratelimit/ratelimit.go",
		Old: "redis.call('SET', KEYS[2], settings, 'PX', math.floor(window / 1000) + 1)", New: "redis.call('SET', KEYS[2], settings)",
		Package: "./internal/ratelimit", Test: "TestTheWindowHoldsAtMostTheLimitAndExpires",
	},
	{
		Name: "ratelimit: Wait refuses a worker with other settings", File: "internal/ratelimit/ratelimit.go",
		Old: "if stored and stored ~= settings then", New: "if false then",
		Package: "./internal/ratelimit", Test: "TestWorkersWithDifferentSettingsAreRefused",
	},
	{
		Name: "ratelimit: Available refuses a worker with other settings", File: "internal/ratelimit/ratelimit.go",
		Old: "if held and held ~= ARGV[1]", New: "if false and held ~= ARGV[1]",
		Package: "./internal/ratelimit", Test: "TestWorkersWithDifferentSettingsAreRefused",
	},
	{
		Name: "ratelimit: one window per channel", File: "internal/ratelimit/ratelimit.go",
		Old: "return \"ratelimit:\" + string(ch) }", New: "return \"ratelimit:\" }",
		Package: "./internal/ratelimit", Test: "TestChannelsHaveSeparateWindows",
	},
	{
		Name: "ratelimit: no worker time is sent to Redis (I12)", File: "internal/ratelimit/ratelimit.go",
		Old: "l.limit, l.window.Microseconds(), uuid.NewString()).Int64Slice()", New: "l.limit, l.window.Microseconds(), uuid.NewString(), time.Now().UnixMicro()).Int64Slice()",
		Package: "./internal/ratelimit", Test: "TestTheLimiterSendsRedisNoClockTime",
	},
	{
		Name: "ratelimit: the limit is at least 1", File: "internal/ratelimit/ratelimit.go",
		Old: "if limit < 1 {", New: "if false && limit < 1 {",
		Package: "./internal/ratelimit", Test: "TestNewRefusesALimitBelowOneAndAMarginThatIsNotPositive",
	},
	{
		Name: "ratelimit: the margin is positive", File: "internal/ratelimit/ratelimit.go",
		Old: "if margin <= 0 {", New: "if margin < 0 {",
		Package: "./internal/ratelimit", Test: "TestNewRefusesALimitBelowOneAndAMarginThatIsNotPositive",
	},

	// Session 4: the provider client and the send gate of architecture §7 (I8, A1, A2, A4, I1 at
	// the provider's side), and the bounds on what it reads from a response (§8).
	{
		Name: "provider: the gate runs when the connection is ready", File: "internal/provider/provider.go",
		Old: "GotConn: onConn}", New: "GetConn: func(string) { onConn(httptrace.GotConnInfo{}) }}",
		Package: "./internal/provider", Test: "TestTheGateRunsOnceWhenTheConnectionIsReadyAndBeforeTheWrite",
	},
	{
		Name: "provider: the gate runs when the connection is ready (A4)", File: "internal/provider/provider.go",
		Old: "GotConn: onConn}", New: "GetConn: func(string) { onConn(httptrace.GotConnInfo{}) }}",
		Package: "./internal/provider", Test: "TestPermitToArrivalStaysWithinTheMargin",
	},
	{
		Name: "provider: the gate runs before the write", File: "internal/provider/provider.go",
		Old: "GotConn: onConn}", New: "WroteRequest: func(httptrace.WroteRequestInfo) { onConn(httptrace.GotConnInfo{}) }}",
		Package: "./internal/provider", Test: "TestTheGateRunsOnceWhenTheConnectionIsReadyAndBeforeTheWrite",
	},
	{
		Name: "provider: every send passes the gate (I1)", File: "internal/provider/provider.go",
		Old: "if err := gate(gateCtx); err != nil {", New: "if err := gateCtx.Err(); err != nil {",
		Package: "./internal/provider", Test: "TestTheProviderReceivesAtMostTheLimitInAnyWindow",
	},
	{
		Name: "provider: a refused send is not sent (A1)", File: "internal/provider/provider.go",
		Old: "if err := gate(gateCtx); err != nil {", New: "if err := gate(gateCtx); err != nil && false {",
		Package: "./internal/provider", Test: "TestARefusedSendNeverReachesTheProvider",
	},
	{
		Name: "provider: a refused send's connection is closed in the hook", File: "internal/provider/provider.go",
		Old: "_ = info.Conn.Close() //", New: "_ = info.Conn //",
		Package: "./internal/provider", Test: "TestARefusedSendsConnectionIsClosedBeforeTheTransportGoesOn",
	},
	{
		Name: "provider: the Result says why the gate refused", File: "internal/provider/provider.go",
		Old: "r.Refusal = err", New: "_ = err",
		Package: "./internal/provider", Test: "TestTheResultCarriesWhatWasObserved",
	},
	{
		Name: "provider: no replay on a dead connection (A2)", File: "internal/provider/provider.go",
		Old: "req.GetBody = nil //", New: "_ = req.GetBody //",
		Package: "./internal/provider", Test: "TestADeadReusedConnectionIsNotReplayed",
	},
	{
		Name: "provider: redirects are not followed", File: "internal/provider/provider.go",
		Old: "CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },", New: "CheckRedirect: nil,",
		Package: "./internal/provider", Test: "TestRedirectsAreNotFollowed",
	},
	{
		Name: "provider: HTTP/1.1 only", File: "internal/provider/provider.go",
		Old: "protocols.SetHTTP1(true)", New: "protocols.SetHTTP1(true); protocols.SetHTTP2(true)",
		Package: "./internal/provider", Test: "TestTheClientSpeaksHTTP11ToAServerThatOffersHTTP2",
	},
	{
		Name: "provider: one deadline per send", File: "internal/provider/provider.go",
		Old: "ctx, cancel := context.WithTimeout(ctx, c.sendTimeout)", New: "ctx, cancel := context.WithCancel(ctx)",
		Package: "./internal/provider", Test: "TestOneDeadlineCoversConnectGateWriteAndResponse",
	},
	{
		Name: "provider: the gate waits within the send's deadline", File: "internal/provider/provider.go",
		Old: "gateCtx, gateCancel := context.WithTimeout(ctx, c.gateBudget)", New: "gateCtx, gateCancel := context.WithTimeout(context.Background(), c.gateBudget)",
		Package: "./internal/provider", Test: "TestAGateThatWaitsPastTheDeadlineTakesNoPermit",
	},
	{
		Name: "provider: the TLS handshake has its own timeout", File: "internal/provider/provider.go",
		Old: "TLSHandshakeTimeout:    o.TLSHandshakeTimeout,", New: "TLSHandshakeTimeout:    0,",
		Package: "./internal/provider", Test: "TestTheTLSHandshakeHasItsOwnTimeout",
	},
	{
		Name: "provider: an idle connection for every slot", File: "internal/provider/provider.go",
		Old: "MaxIdleConnsPerHost:    o.IdleConns,", New: "MaxIdleConnsPerHost:    0,",
		Package: "./internal/provider", Test: "TestTheClientKeepsAnIdleConnectionForEverySlot",
	},
	{
		Name: "provider: idle connections are dropped after the timeout", File: "internal/provider/provider.go",
		Old: "IdleConnTimeout:        o.IdleConnTimeout,", New: "IdleConnTimeout:        0,",
		Package: "./internal/provider", Test: "TestIdleConnectionsAreDroppedAfterTheIdleTimeout",
	},
	{
		Name: "provider: response headers are bounded", File: "internal/provider/provider.go",
		Old: "MaxResponseHeaderBytes: MaxResponseHeaderBytes,", New: "MaxResponseHeaderBytes: 0,",
		Package: "./internal/provider", Test: "TestResponseHeadersOverTheBoundAreNotAResponse",
	},
	{
		Name: "provider: the response body is read up to its bound", File: "internal/provider/provider.go",
		Old: "io.LimitReader(resp.Body, MaxResponseBody)", New: "io.LimitReader(resp.Body, 1<<62)",
		Package: "./internal/provider", Test: "TestTheResponseBodyIsReadUpToItsBound",
	},
	{
		Name: "provider: Retry-After is capped", File: "internal/provider/provider.go",
		Old: "err == nil && secs > int64(MaxRetryAfter/time.Second):", New: "false:",
		Package: "./internal/provider", Test: "TestTheResultCarriesWhatWasObserved",
	},
	{
		Name: "provider: the notification id is the Idempotency-Key", File: "internal/provider/provider.go",
		Old: "req.Header.Set(\"Idempotency-Key\", m.ID.String())", New: "_ = m.ID.String()",
		Package: "./internal/provider", Test: "TestTheRequestHasTheAssignmentsFormat",
	},
	{
		Name: "provider: a 101's connection is not read (review F2)", File: "internal/provider/provider.go",
		Old: "if resp.StatusCode < http.StatusOK {", New: "if false && resp.StatusCode < http.StatusOK {",
		Package: "./internal/provider", Test: "TestOneDeadlineCoversConnectGateWriteAndResponse",
	},
	{
		Name: "provider: a Retry-After date in the past is 0", File: "internal/provider/provider.go",
		Old: "err == nil && t.After(now) {", New: "err == nil {",
		Package: "./internal/provider", Test: "TestTheResultCarriesWhatWasObserved",
	},

	// Session 5: the mock provider's fault injection (cmd/mockprovider).
	{
		Name: "mockprovider: injects failures at their rate", File: "cmd/mockprovider/main.go",
		Old: "case random() < s.failRate:", New: "case false && random() < s.failRate:",
		Package: "./cmd/mockprovider", Test: "TestMockProviderInjectsFailuresAndThrottlesAtTheirRates",
	},
	{
		Name: "mockprovider: a rate is between 0 and 1", File: "cmd/mockprovider/main.go",
		Old: "if err != nil || !(f >= 0 && f <= 1) {", New: "if err != nil {",
		Package: "./cmd/mockprovider", Test: "TestMockProviderRefusesARateOutsideZeroToOne",
	},
}
