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
		Old: "ReadHeaderTimeout: t.header,", New: "ReadHeaderTimeout: 0,",
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
		Package: "./internal/ratelimit", Test: "TestAvailableFailsWhenTheTakeScriptWould",
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
		Old: "local used = redis.call('ZCOUNT', KEYS[1], string.format('(%d', now - tonumber(ARGV[2])), '+inf')", New: "redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', string.format('%d', now - tonumber(ARGV[2])))\nlocal used = redis.call('ZCARD', KEYS[1])",
		Package: "./internal/ratelimit", Test: "TestAvailableChangesNothing",
	},
	{
		Name: "ratelimit: Available fails when Redis refuses writes (session 5's review, F2)", File: "internal/ratelimit/ratelimit.go",
		Old: "var readScript = redis.NewScript(`#!lua\n", New: "var readScript = redis.NewScript(`#!lua flags=no-writes\n",
		Package: "./internal/ratelimit", Test: "TestAvailableFailsWhenTheTakeScriptWould",
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

	// Session 5: the dispatcher of architecture §6 (the loop, Classify, the retry policy, the
	// breaker and the reaper), the provider client's changes of §6 and §7, and the worker.
	{
		Name: "dispatch: claims the highest priority first (I9)", File: "internal/dispatch/dispatch.go",
		Old: "for _, p := range domain.Priorities {", New: "for _, p := range []domain.Priority{domain.Low, domain.Normal, domain.High} {",
		Package: "./internal/dispatch", Test: "TestHighPriorityOvertakesBacklog",
	},
	{
		Name: "dispatch: one claim batch waits for permits at a time (I9)", File: "internal/dispatch/dispatch.go",
		Old: "if d.waiting.Load() > 0 {", New: "if false && d.waiting.Load() > 0 {",
		Package: "./internal/dispatch", Test: "TestAWorkerHasAtMostOneClaimBatchWaitingForPermits",
	},
	{
		Name: "dispatch: a claim takes at most the claim batch", File: "internal/dispatch/dispatch.go",
		Old: "n := min(cap(d.slots)-len(d.slots), d.o.ClaimBatch)", New: "n := cap(d.slots) - len(d.slots)",
		Package: "./internal/dispatch", Test: "TestAWorkerHasAtMostOneClaimBatchWaitingForPermits",
	},
	{
		Name: "dispatch: room reads the breaker", File: "internal/dispatch/dispatch.go",
		Old: "n = min(n, d.breaker.Allow())", New: "n = min(n, n)",
		Package: "./internal/dispatch", Test: "TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue",
	},
	{
		Name: "dispatch: room reads Available", File: "internal/dispatch/dispatch.go",
		Old: "n = min(n, free)", New: "n = min(n, n+0*free)",
		Package: "./internal/dispatch", Test: "TestNoRowIsClaimedWhileTheWindowIsFull",
	},
	{
		Name: "dispatch: the gate is the limiter (I1)", File: "internal/dispatch/dispatch.go",
		Old: "err := d.limiter.Wait(gateCtx, d.o.Channel)", New: "err := gateCtx.Err()",
		Package: "./internal/dispatch", Test: "TestTheProviderReceivesAtMostTheLimitThroughTheDispatchers",
	},
	{
		Name: "dispatch: the breaker learns from Classify", File: "internal/dispatch/dispatch.go",
		Old: "d.breaker.Report(probe, verdict.Lesson)", New: "d.breaker.Report(probe, Nothing)",
		Package: "./internal/dispatch", Test: "TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue",
	},
	{
		Name: "dispatch: the probe's report is the probe's", File: "internal/dispatch/dispatch.go",
		Old: "d.breaker.Report(probe, verdict.Lesson)", New: "d.breaker.Report(false, verdict.Lesson)",
		Package: "./internal/dispatch", Test: "TestTheHalfOpenBreakerLetsExactlyOneRowThrough",
	},
	{
		Name: "dispatch: a started row in half-open is the probe", File: "internal/dispatch/dispatch.go",
		Old: "probe := d.breaker.Started()", New: "probe := false",
		Package: "./internal/dispatch", Test: "TestTheHalfOpenBreakerLetsExactlyOneRowThrough",
	},
	{
		Name: "dispatch: the single exit ends in Finish (I4)", File: "internal/dispatch/dispatch.go",
		Old: "d.finish(ctx, row, result, verdict)", New: "_ = row",
		Package: "./internal/dispatch", Test: "TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem",
	},
	{
		Name: "dispatch: sends run without the loop's cancellation", File: "internal/dispatch/dispatch.go",
		Old: "go d.handle(context.WithoutCancel(ctx), row, probe, wait)", New: "go d.handle(ctx, row, probe, wait)",
		Package: "./internal/dispatch", Test: "TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem",
	},
	{
		Name: "dispatch: Run waits for the rows in flight", File: "internal/dispatch/dispatch.go",
		Old: "defer d.inflight.Wait()", New: "defer func() {}()",
		Package: "./internal/dispatch", Test: "TestShutdownWaitsForSendsInFlightAndDoesNotCancelThem",
	},
	{
		Name: "dispatch: a result write is retried until answered (lessons 7)", File: "internal/dispatch/dispatch.go",
		Old: "if time.Now().Add(delay).After(giveUp) {", New: "if time.Now().Add(delay).After(giveUp) || true {",
		Package: "./internal/dispatch", Test: "TestAFailedResultWriteIsRetriedAndNotSentAgain",
	},
	{
		Name: "dispatch: a retry waits its delay", File: "internal/dispatch/dispatch.go",
		Old: "RetryIn: d.o.Retry.DelayAfter(row.Attempt, v)", New: "RetryIn: 0",
		Package: "./internal/dispatch", Test: "TestARetriedRowWaitsForItsBackoff",
	},
	{
		Name: "classify: a response always wins (I6)", File: "internal/dispatch/classify.go",
		Old: "case r.Responded && r.StatusCode >= 200 && r.StatusCode < 300:", New: "case r.Refusal == nil && r.Responded && r.StatusCode >= 200 && r.StatusCode < 300:",
		Package: "./internal/dispatch", Test: "TestAResponseAlwaysWins",
	},
	{
		Name: "classify: 400, 413 and 422 end the message", File: "internal/dispatch/classify.go",
		Old: "return code == http.StatusBadRequest ||", New: "return false && code == http.StatusBadRequest ||",
		Package: "./internal/dispatch", Test: "TestClassifyDecidesEveryRowOfTheVerdictTable",
	},
	{
		Name: "classify: 404 and 401 are failures of the channel", File: "internal/dispatch/classify.go",
		Old: "return code == http.StatusBadRequest ||", New: "return code >= 400 && code < 500 ||",
		Package: "./internal/dispatch", Test: "TestNotFoundAndUnauthorizedOpenTheBreakerAndDoNotDrainTheQueue",
	},
	{
		Name: "classify: a limiter error is a failure", File: "internal/dispatch/classify.go",
		Old: "case r.Refusal != nil && !errors.Is(r.Refusal, ratelimit.ErrNoPermitInTime):", New: "case false && r.Refusal != nil && !errors.Is(r.Refusal, ratelimit.ErrNoPermitInTime):",
		Package: "./internal/dispatch", Test: "TestClassifyDecidesEveryRowOfTheVerdictTable",
	},
	{
		Name: "classify: no permit in time teaches nothing", File: "internal/dispatch/classify.go",
		Old: "Delay: Soon, Lesson: Nothing, Outcome: \"no permit in time\"", New: "Delay: Soon, Lesson: Failure, Outcome: \"no permit in time\"",
		Package: "./internal/dispatch", Test: "TestClassifyDecidesEveryRowOfTheVerdictTable",
	},
	{
		Name: "classify: a connection closed idle teaches nothing", File: "internal/dispatch/classify.go",
		Old: "case r.ClosedIdle:", New: "case false && r.ClosedIdle:",
		Package: "./internal/dispatch", Test: "TestTheVerdictTableHoldsForRealSends",
	},
	{
		Name: "classify: the verdict carries Retry-After", File: "internal/dispatch/classify.go",
		Old: "RetryAfter: r.RetryAfter,", New: "RetryAfter: 0,",
		Package: "./internal/dispatch", Test: "TestClassifyDecidesEveryRowOfTheVerdictTable",
	},
	{
		Name: "retry: the ceiling doubles", File: "internal/dispatch/retry.go",
		Old: "ceiling *= 2", New: "ceiling += p.Base",
		Package: "./internal/dispatch", Test: "TestRetryDelaysDoubleWithJitterUpToTheCap",
	},
	{
		Name: "retry: no delay exceeds the cap", File: "internal/dispatch/retry.go",
		Old: "ceiling = min(ceiling, p.Cap)", New: "ceiling = max(ceiling, 0)",
		Package: "./internal/dispatch", Test: "TestRetryDelaysDoubleWithJitterUpToTheCap",
	},
	{
		Name: "retry: delays are jittered", File: "internal/dispatch/retry.go",
		Old: "d := half + time.Duration(random()*float64(ceiling-half))", New: "d := ceiling + 0*half",
		Package: "./internal/dispatch", Test: "TestRetryDelaysDoubleWithJitterUpToTheCap",
	},
	{
		Name: "retry: Retry-After is honoured", File: "internal/dispatch/retry.go",
		Old: "return max(d, min(v.RetryAfter, p.Cap))", New: "return d",
		Package: "./internal/dispatch", Test: "TestRetryAfterIsHonouredUpToTheCap",
	},
	{
		Name: "retry: Retry-After is capped", File: "internal/dispatch/retry.go",
		Old: "return max(d, min(v.RetryAfter, p.Cap))", New: "return max(d, v.RetryAfter)",
		Package: "./internal/dispatch", Test: "TestRetryAfterIsHonouredUpToTheCap",
	},
	{
		Name: "retry: soon is about one window", File: "internal/dispatch/retry.go",
		Old: "return p.Soon + time.Duration(random()*float64(p.Soon))", New: "return time.Duration(random() * float64(p.Soon))",
		Package: "./internal/dispatch", Test: "TestRetryDelaysDoubleWithJitterUpToTheCap",
	},
	{
		Name: "breaker: five failures in a row open it", File: "internal/dispatch/breaker.go",
		Old: "if b.failures++; b.failures >= FailuresToOpen {", New: "if b.failures++; b.failures >= FailuresToOpen+1 {",
		Package: "./internal/dispatch", Test: "TestBreakerOpensAfterFiveFailuresInARow",
	},
	{
		Name: "breaker: a success resets the count", File: "internal/dispatch/breaker.go",
		Old: "case b.state() == Closed && l == Success:\n\t\tb.failures = 0", New: "case b.state() == Closed && l == Success:\n\t\t_ = 0",
		Package: "./internal/dispatch", Test: "TestBreakerOpensAfterFiveFailuresInARow",
	},
	{
		Name: "breaker: open lasts its open time", File: "internal/dispatch/breaker.go",
		Old: "case b.now().Sub(b.openedAt) < b.open:", New: "case false:",
		Package: "./internal/dispatch", Test: "TestBreakerOpensAfterFiveFailuresInARow",
	},
	{
		Name: "breaker: one probe at a time", File: "internal/dispatch/breaker.go",
		Old: "\t\tif !b.probing {\n\t\t\treturn 1", New: "\t\tif true {\n\t\t\treturn 1",
		Package: "./internal/dispatch", Test: "TestBreakerHalfOpenLetsOneProbeThrough",
	},
	{
		Name: "breaker: only half-open marks a probe", File: "internal/dispatch/breaker.go",
		Old: "if b.state() == HalfOpen && !b.probing {", New: "if !b.probing {",
		Package: "./internal/dispatch", Test: "TestBreakerHalfOpenLetsOneProbeThrough",
	},
	{
		Name: "breaker: only the probe's report counts outside closed", File: "internal/dispatch/breaker.go",
		Old: "case probe && b.probing:", New: "case b.probing:",
		Package: "./internal/dispatch", Test: "TestBreakerHalfOpenLetsOneProbeThrough",
	},
	{
		Name: "breaker: a failed probe opens it again", File: "internal/dispatch/breaker.go",
		Old: "\t\tcase Failure:\n\t\t\tb.openedAt = b.now()", New: "\t\tcase Failure:\n\t\t\t_ = b.now()",
		Package: "./internal/dispatch", Test: "TestBreakerHalfOpenLetsOneProbeThrough",
	},
	{
		Name: "breaker: a refused probe leaves it half-open", File: "internal/dispatch/breaker.go",
		Old: "\tcase probe && b.probing:\n\t\tb.probing = false", New: "\tcase probe && b.probing:\n\t\tb.probing = l == Nothing",
		Package: "./internal/dispatch", Test: "TestARefusedProbeLeavesTheBreakerHalfOpen",
	},
	{
		Name: "reaper: batches until a short one", File: "internal/dispatch/reaper.go",
		Old: "if len(changes) < ReapBatch {", New: "if true {",
		Package: "./internal/dispatch", Test: "TestTheReaperReturnsExpiredRowsAndPublishesTheirStatus",
	},
	{
		Name: "reaper: publishes each change", File: "internal/dispatch/reaper.go",
		Old: "\t\t\tr.Events.Publish(rowCtx, events.Event{ID: c.ID, BatchID: c.BatchID, Status: c.Status})\n", New: "",
		Package: "./internal/dispatch", Test: "TestTheReaperReturnsExpiredRowsAndPublishesTheirStatus",
	},
	{
		Name: "provider: a connection closed idle is reported", File: "internal/provider/provider.go",
		Old: "r.ClosedIdle = r.Refusal == nil && r.ConnReused && closedIdle(err)", New: "r.ClosedIdle = false && r.ConnReused && closedIdle(err)",
		Package: "./internal/provider", Test: "TestAConnectionClosedByTheProviderAtTheGateIsReportedAsClosedIdle",
	},
	{
		Name: "provider: a URL that cannot make a request is refused", File: "internal/provider/provider.go",
		Old: "err != nil || (u.Scheme != \"http\" && u.Scheme != \"https\") || u.Host == \"\"", New: "err != nil || u == nil",
		Package: "./internal/provider", Test: "TestNewRefusesAURLThatIsNotAbsoluteHTTP",
	},
	{
		Name: "provider: the correlation id is sent only within its bound", File: "internal/provider/provider.go",
		Old: "if observability.ValidCorrelationID(m.CorrelationID) {", New: "if observability.ValidCorrelationID(m.CorrelationID) || m.CorrelationID != \"\" {",
		Package: "./internal/provider", Test: "TestACorrelationIDOutsideItsBoundIsNotSent",
	},
	{
		Name: "worker: a dispatcher runs per channel", File: "cmd/worker/main.go",
		Old: "running.Go(func() { d.Run(ctx) })", New: "_ = d",
		Package: "./cmd/worker", Test: "TestTheWorkerDeliversEveryChannel",
	},
	{
		Name: "worker: a reaper runs", File: "cmd/worker/main.go",
		Old: "running.Go(func() { reaper.Run(ctx) })", New: "_ = reaper",
		Package: "./cmd/worker", Test: "TestAKilledWorkersRowComesBackThroughTheReaper",
	},
	{
		Name: "provider: ClosedIdle only on a reused connection (session 5's review, F1)", File: "internal/provider/provider.go",
		Old: "r.ClosedIdle = r.Refusal == nil && r.ConnReused && closedIdle(err)", New: "r.ClosedIdle = r.Refusal == nil && closedIdle(err)",
		Package: "./internal/provider", Test: "TestAConnectionClosedByTheProviderAtTheGateIsReportedAsClosedIdle",
	},
	{
		Name: "provider: New's error does not quote the URL", File: "internal/provider/provider.go",
		Old: "return nil, errors.New(\"provider: the URL is not an absolute http or https URL with a host\")", New: "return nil, fmt.Errorf(\"provider: %q is not an absolute http or https URL with a host\", o.URL)",
		Package: "./internal/provider", Test: "TestNewRefusesAURLThatIsNotAbsoluteHTTP",
	},
	{
		Name: "worker: logs only the provider's host", File: "cmd/worker/main.go",
		Old: "log.Info(\"worker started\", \"provider\", providerHost(cfg.ProviderURL),", New: "log.Info(\"worker started\", \"provider\", cfg.ProviderURL, \"host\", providerHost(cfg.ProviderURL),",
		Package: "./cmd/worker", Test: "TestTheWorkerLogsOnlyTheProvidersHost",
	},

	// Session 6: the API of architecture §8.
	{
		Name: "domain: content is bounded per channel", File: "internal/domain/request.go",
		Old: "} else if p.Channel != \"\" && chars > ContentLimit(p.Channel) {", New: "} else if false && p.Channel != \"\" && chars > ContentLimit(p.Channel) {",
		Package: "./internal/domain", Test: "TestContentIsBoundedPerChannelInCharacters",
	},
	{
		Name: "domain: content is counted in characters", File: "internal/domain/request.go",
		Old: "chars, p.Bytes = utf8.RuneCountInString(p.content), len(p.content)", New: "chars, p.Bytes = len(p.content), len(p.content)",
		Package: "./internal/domain", Test: "TestContentIsBoundedPerChannelInCharacters",
	},
	{
		Name: "domain: content must not contain NUL", File: "internal/domain/request.go",
		Old: "if strings.ContainsRune(p.content, 0) {", New: "if false {",
		Package: "./internal/domain", Test: "TestContentAndVariablesMustNotContainNUL",
	},
	{
		Name: "domain: variable values must not contain NUL", File: "internal/domain/request.go",
		Old: "if strings.ContainsRune(v.Value, 0) {", New: "if false {",
		Package: "./internal/domain", Test: "TestContentAndVariablesMustNotContainNUL",
	},
	{
		Name: "domain: variables are counted while decoded", File: "internal/domain/request.go",
		Old: "if len(out) == MaxVariables {", New: "if false {",
		Package: "./internal/domain", Test: "TestVariablesAreCountedWhileDecoded",
	},
	{
		Name: "domain: a variable name appears once", File: "internal/domain/request.go",
		Old: "if err != nil || !ok1 || !ok2 || seen[n] {", New: "if err != nil || !ok1 || !ok2 {",
		Package: "./internal/domain", Test: "TestVariablesAreCountedWhileDecoded",
	},
	{
		Name: "domain: every {{ starts a placeholder", File: "internal/domain/request.go",
		Old: "if end < 0 || !strings.HasPrefix(rest, \"{{.\") {", New: "if end < 0 {",
		Package: "./internal/domain", Test: "TestVariableNamesAreBounded",
	},
	{
		Name: "domain: variable names are bounded", File: "internal/domain/request.go",
		Old: "if !variableName.MatchString(name) || len(name) > MaxVariableNameLen {", New: "if !variableName.MatchString(name) {",
		Package: "./internal/domain", Test: "TestVariableNamesAreBounded",
	},
	{
		Name: "domain: a template has at most 50 variables", File: "internal/domain/request.go",
		Old: "if len(c.vars) == MaxVariables {", New: "if false {",
		Package: "./internal/domain", Test: "TestTemplateBodyBounds",
	},
	{
		Name: "domain: a template body is bounded by its channel", File: "internal/domain/request.go",
		Old: "case ch != \"\" && utf8.RuneCountInString(body) > ContentLimit(ch):", New: "case false:",
		Package: "./internal/domain", Test: "TestTemplateBodyBounds",
	},
	{
		Name: "domain: a variable the template does not use is an error", File: "internal/domain/request.go",
		Old: "if uses == 0 {", New: "if false {",
		Package: "./internal/domain", Test: "TestTheRenderedLengthIsMeasuredBeforeRendering",
	},
	{
		Name: "domain: a placeholder without a variable is an error", File: "internal/domain/request.go",
		Old: "if !given[name] {", New: "if false {",
		Package: "./internal/domain", Test: "TestTheRenderedLengthIsMeasuredBeforeRendering",
	},
	{
		Name: "domain: a template's channel is the notification's", File: "internal/domain/request.go",
		Old: "if p.Channel != \"\" && ch != p.Channel {", New: "if false && p.Channel != \"\" && ch != p.Channel {",
		Package: "./internal/domain", Test: "TestATemplateItemIsMeasuredAgainstItsChannel",
	},
	{
		Name: "domain: the hash separates fields", File: "internal/domain/request.go",
		Old: "_ = binary.Write(h, binary.BigEndian, uint64(len(s)))", New: "_ = binary.BigEndian",
		Package: "./internal/domain", Test: "TestTheRequestHashIgnoresFormAndDefaults",
	},
	{
		Name: "domain: the hash fills in the default priority", File: "internal/domain/request.go",
		Old: "priority = Normal.String()", New: "priority = \"\"",
		Package: "./internal/domain", Test: "TestTheRequestHashIgnoresFormAndDefaults",
	},
	{
		Name: "domain: scheduledAt is read in UTC", File: "internal/domain/request.go",
		Old: "t = t.UTC().Truncate(time.Microsecond)", New: "t = t.Truncate(time.Microsecond)",
		Package: "./internal/domain", Test: "TestTheRequestHashIgnoresFormAndDefaults",
	},
	{
		Name: "domain: the hash sorts the variables", File: "internal/domain/request.go",
		Old: "slices.SortFunc(vars, func(a, b Variable) int { return strings.Compare(a.Name, b.Name) })", New: "_ = vars",
		Package: "./internal/domain", Test: "TestTheRequestHashIgnoresFormAndDefaults",
	},
	{
		Name: "domain: field errors are kept up to 100", File: "internal/domain/request.go",
		Old: "if len(e.list) < MaxFieldErrors {", New: "if true {",
		Package: "./internal/domain", Test: "TestFieldErrorsAreKeptUpToTheirBound",
	},
	{
		Name: "domain: an SMS recipient is E.164", File: "internal/domain/request.go",
		Old: "if !e164.MatchString(to) {", New: "if false {",
		Package: "./internal/domain", Test: "TestRecipientFormatsPerChannel",
	},
	{
		Name: "domain: an email recipient is at most 254 bytes", File: "internal/domain/request.go",
		Old: "if len(to) > MaxEmailRecipientBytes || err != nil", New: "if err != nil",
		Package: "./internal/domain", Test: "TestRecipientFormatsPerChannel",
	},
	{
		Name: "domain: a device token is bounded", File: "internal/domain/request.go",
		Old: "if len(to) < MinDeviceTokenLen || len(to) > MaxDeviceTokenLen || !deviceToken.MatchString(to) {", New: "if !deviceToken.MatchString(to) {",
		Package: "./internal/domain", Test: "TestRecipientFormatsPerChannel",
	},
	{
		Name: "domain: an idempotency key is printable ASCII", File: "internal/domain/request.go",
		Old: "if s[i] < 0x21 || s[i] > 0x7e {", New: "if false {",
		Package: "./internal/domain", Test: "TestIdempotencyKeyAndTemplateNameBounds",
	},
	{
		Name: "domain: an idempotency key is at most 128", File: "internal/domain/request.go",
		Old: "if len(s) == 0 || len(s) > MaxIdempotencyKeyLen {", New: "if len(s) == 0 {",
		Package: "./internal/domain", Test: "TestIdempotencyKeyAndTemplateNameBounds",
	},
	{
		Name: "domain: a template name is at most 64", File: "internal/domain/request.go",
		Old: "return len(s) > 0 && len(s) <= MaxTemplateNameLen && templateName.MatchString(s)", New: "return len(s) > 0 && templateName.MatchString(s)",
		Package: "./internal/domain", Test: "TestIdempotencyKeyAndTemplateNameBounds",
	},
	{
		Name: "domain: a name from the request is cut to 64 bytes", File: "internal/domain/request.go",
		Old: "if len(name) > MaxEchoedName {", New: "if false {",
		Package: "./internal/api", Test: "TestErrorBodiesStayWithinTheirBoundForAnyRequest",
	},
	{
		Name: "domain: a variable name in an error passes Echo", File: "internal/domain/request.go",
		Old: "errs.Add(at(\"variables.\"+Echo(ve.Name)), ve.Message)", New: "errs.Add(at(\"variables.\"+ve.Name), ve.Message)",
		Package: "./internal/api", Test: "TestErrorBodiesStayWithinTheirBoundForAnyRequest",
	},
	{
		Name: "api: the body is bounded at its limit", File: "internal/api/notifications.go",
		Old: "rd := http.MaxBytesReader(w, r.Body, limit)", New: "rd := http.MaxBytesReader(w, r.Body, limit+1)",
		Package: "./internal/api", Test: "TestRequestBodiesAreBoundedAtTheirLimit",
	},
	{
		Name: "api: a batch is decoded up to 1000 items", File: "internal/api/notifications.go",
		Old: "if len(items) == domain.MaxBatchItems {", New: "if false {",
		Package: "./internal/api", Test: "TestBatchSizeIsBoundedAtAThousand",
	},
	{
		Name: "api: 8 MiB of empty items is not decoded", File: "internal/api/notifications.go",
		Old: "if len(items) == domain.MaxBatchItems {", New: "if false {",
		Package: "./internal/api", Test: "TestABatchOfManyEmptyItemsIsRefusedWithoutDecodingThemAll",
	},
	{
		Name: "api: the rendered total is bounded", File: "internal/api/notifications.go",
		Old: "if total > domain.MaxRenderedBytes {", New: "if false && total > domain.MaxRenderedBytes {",
		Package: "./internal/api", Test: "TestTheRenderedTotalOfARequestIsBounded",
	},
	{
		Name: "api: the rendered total's bound is inclusive", File: "internal/api/notifications.go",
		Old: "if total > domain.MaxRenderedBytes {", New: "if total >= domain.MaxRenderedBytes {",
		Package: "./internal/api", Test: "TestTheRenderedTotalOfARequestIsBounded",
	},
	{
		Name: "api: a template is compiled once per request", File: "internal/api/notifications.go",
		Old: "if t.compiled == nil {", New: "if true {",
		Package: "./internal/api", Test: "TestATemplateIsCompiledOncePerRequest",
	},
	{
		Name: "domain: an unknown field's name passes Echo", File: "internal/domain/request.go",
		Old: "return &FieldNameError{Name: Echo(name), Message: \"is not a known field; names are matched exactly\"}", New: "return &FieldNameError{Name: name, Message: \"is not a known field; names are matched exactly\"}",
		Package: "./internal/api", Test: "TestErrorBodiesStayWithinTheirBoundForAnyRequest",
	},
	{
		Name: "domain: an unknown field is refused", File: "internal/domain/request.go",
		Old: "case !slices.Contains(allowed, name):", New: "case false:",
		Package: "./internal/domain", Test: "TestFieldNamesAreMatchedExactlyAndOnce",
	},
	{
		Name: "api: nothing follows the JSON value", File: "internal/api/notifications.go",
		Old: "if _, err := dec.Token(); !errors.Is(err, io.EOF) {", New: "if _, err := dec.Token(); false && !errors.Is(err, io.EOF) {",
		Package: "./internal/api", Test: "TestUnknownFieldsAndWrongTypesAreRefusedByName",
	},
	{
		Name: "api: a batch item has no key of its own", File: "internal/api/notifications.go",
		Old: "if in.IdempotencyKey != \"\" {\n\t\t\terrs.Add(", New: "if false {\n\t\t\terrs.Add(",
		Package: "./internal/api", Test: "TestABatchIsValidatedWholeBeforeAnyIsStored",
	},
	{
		Name: "api: a wrong API key is refused", File: "internal/api/notifications.go",
		Old: "if given == \"\" || match != 1 {", New: "if given == \"\" {",
		Package: "./internal/api", Test: "TestAPIKeysAreRequiredWhenConfigured",
	},
	{
		Name: "api: /retry gives MAX_ATTEMPTS more attempts", File: "internal/api/notifications.go",
		Old: "return s.queue.Requeue(r.Context(), id, s.maxAttempts)", New: "return s.queue.Requeue(r.Context(), id, 1)",
		Package: "./internal/api", Test: "TestRetryRequeuesOnlyAFailedNotification",
	},
	{
		Name: "api: a refused cancel reports the status found", File: "internal/api/notifications.go",
		Old: "Message: \"the notification is not in a status that allows this\", Status: string(st)}", New: "Message: \"the notification is not in a status that allows this\"}",
		Package: "./internal/api", Test: "TestCancelOnlyFromWaitingStatusesThroughTheAPI",
	},
	{
		Name: "api: a created notification is published", File: "internal/api/notifications.go",
		Old: "\t\ts.events.Publish(r.Context(), events.Event{ID: n.ID, Status: n.Status})\n", New: "",
		Package: "./internal/api", Test: "TestCreateAndReadANotification",
	},
	{
		Name: "api: the list page is at most 100", File: "internal/api/notifications.go",
		Old: "if err != nil || n < 1 || n > MaxPage {", New: "if err != nil || n < 1 {",
		Package: "./internal/api", Test: "TestTheListIsOrderedAndPagedByCursor",
	},
	{
		Name: "api: every route is in the specification", File: "internal/api/notifications.go",
		Old: "\t\t{\"GET /v1/templates/{name}\", s.template},\n", New: "",
		Package: "./internal/api", Test: "TestOpenAPIMatchesTheServer",
	},
	{
		Name: "store: a repeated key reads the first row", File: "internal/store/notifications.go",
		Old: "ON CONFLICT (idempotency_key) DO NOTHING\nRETURNING ` + notificationColumns", New: "RETURNING ` + notificationColumns",
		Package: "./internal/api", Test: "TestConcurrentIdenticalRequestsCreateOneNotification",
	},
	{
		Name: "store: a key with another request is a conflict", File: "internal/store/notifications.go",
		Old: "\tif !bytes.Equal(hash, n.RequestHash) {\n\t\treturn existing, false, ErrIdempotencyConflict", New: "\tif len(hash) < 0 {\n\t\treturn existing, false, ErrIdempotencyConflict",
		Package: "./internal/api", Test: "TestAKeyReusedWithAnotherRequestIsAConflict",
	},
	{
		Name: "store: a repeated batch key reads the first batch", File: "internal/store/notifications.go",
		Old: "VALUES ($1, NULLIF($2, ''), $3, $4) ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`", New: "VALUES ($1, NULLIF($2, ''), $3, $4) RETURNING id`",
		Package: "./internal/api", Test: "TestConcurrentIdenticalBatchesCreateOneBatch",
	},
	{
		Name: "store: a batch key with another batch is a conflict", File: "internal/store/notifications.go",
		Old: "\t\t\tif !bytes.Equal(hash, b.RequestHash) {", New: "\t\t\tif false {",
		Package: "./internal/api", Test: "TestConcurrentIdenticalBatchesCreateOneBatch",
	},
	{
		Name: "store: batch item ids derive from the batch id", File: "internal/store/notifications.go",
		Old: "return uuid.NewSHA1(batch, []byte(strconv.Itoa(i)))", New: "return uuid.NewSHA1(uuid.New(), []byte(strconv.Itoa(i)))",
		Package: "./internal/api", Test: "TestConcurrentIdenticalBatchesCreateOneBatch",
	},
	{
		Name: "store: a future scheduledAt waits", File: "internal/store/notifications.go",
		Old: "CASE WHEN $6::timestamptz > now() THEN 'scheduled' ELSE 'pending' END,", New: "'pending',",
		Package: "./internal/api", Test: "TestAScheduledNotificationWaitsUntilItsTime",
	},
	{
		Name: "store: the list cursor excludes the last item", File: "internal/store/notifications.go",
		Old: "\"created_at <= $%d AND (created_at < $%d OR (created_at = $%d AND id > $%d))\", c, c, c, i))", New: "\"created_at <= $%d AND (created_at < $%d OR (created_at = $%d AND id >= $%d))\", c, c, c, i))",
		Package: "./internal/api", Test: "TestTheListIsOrderedAndPagedByCursor",
	},
	{
		Name: "store: the list order is the index's", File: "internal/store/notifications.go",
		Old: "sql += fmt.Sprintf(\" ORDER BY created_at DESC, id ASC LIMIT $%d\", len(args))", New: "sql += fmt.Sprintf(\" ORDER BY created_at DESC, id DESC LIMIT $%d\", len(args))",
		Package: "./internal/api", Test: "TestTheListIsOrderedAndPagedByCursor",
	},
	{
		Name: "store: the attempt history is bounded", File: "internal/store/notifications.go",
		Old: "ORDER BY attempt_number DESC LIMIT $2`", New: "ORDER BY attempt_number DESC LIMIT $2 + 1000`",
		Package: "./internal/api", Test: "TestTheAttemptHistoryIsBounded",
	},
	{
		Name: "config: MAX_ATTEMPTS must be positive", File: "internal/config/config.go",
		Old: "if c.MaxAttempts <= 0 {", New: "if false {",
		Package: "./internal/config", Test: "TestMaxAttemptsMustBePositive",
	},
	{
		Name: "api: the whole request, body included, has a timeout", File: "internal/api/health.go",
		Old: "ReadTimeout:       t.request,", New: "",
		Package: "./internal/api", Test: "TestSlowRequestBodiesAreCutOff",
	},
	{
		Name: "api: writing the answer has a timeout", File: "internal/api/health.go",
		Old: "WriteTimeout:      t.answer,", New: "",
		Package: "./internal/api", Test: "TestAnswersToAClientThatDoesNotReadAreCutOff",
	},
	{
		Name: "api: the binary's server has the request timeout", File: "internal/api/health.go",
		Old: "timeouts{header: HeaderTimeout, request: RequestTimeout, answer: AnswerTimeout}", New: "timeouts{header: HeaderTimeout, request: 0, answer: AnswerTimeout}",
		Package: "./internal/api", Test: "TestTheServerHasTheTimeoutsOfTheDesign",
	},
	{
		Name: "api: a cursor's time is within the years a row can have", File: "internal/api/notifications.go",
		Old: "if micros > lastCursorMicros {", New: "if false {",
		Package: "./internal/api", Test: "TestACursorOutsideTheTimeRangeIsRefused",
	},

	// Session 6: the answers to its review.
	{
		Name: "store: the list cursor bounds the index scan (review F3)", File: "internal/store/notifications.go",
		Old: "\"created_at <= $%d AND (created_at < $%d OR", New: "\"created_at <= $%d + interval '100 years' AND (created_at < $%d OR",
		Package: "./internal/store", Test: "TestAListPageReadsTheIndexFromItsCursor",
	},
	{
		Name: "domain: scheduledAt is cut to microseconds (review F4)", File: "internal/domain/request.go",
		Old: "t = t.UTC().Truncate(time.Microsecond)", New: "t = t.UTC()",
		Package: "./internal/domain", Test: "TestTheRequestHashIgnoresFormAndDefaults",
	},
	{
		Name: "domain: scheduledAt is before the year 10000 (review F4)", File: "internal/domain/request.go",
		Old: "if t.Year() > 9999 {", New: "if false {",
		Package: "./internal/domain", Test: "TestAScheduledAtFromTheYear10000IsRefused",
	},
	{
		Name: "domain: a field name is given once (review F7)", File: "internal/domain/request.go",
		Old: "case seen[name]:", New: "case false:",
		Package: "./internal/domain", Test: "TestFieldNamesAreMatchedExactlyAndOnce",
	},
	{
		Name: "domain: a template name starts with a letter or digit", File: "internal/domain/request.go",
		Old: "var templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)", New: "var templateName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)",
		Package: "./internal/domain", Test: "TestIdempotencyKeyAndTemplateNameBounds",
	},
	{
		Name: "domain: the rendered total is the batch's body bound (review F1)", File: "internal/domain/request.go",
		Old: "MaxRenderedBytes       = 8 << 20", New: "MaxRenderedBytes       = 16 << 20",
		Package: "./internal/api", Test: "TestTheRenderedTotalOfARequestIsBounded",
	},
	{
		Name: "api: at most 4 requests render at once (review F1)", File: "internal/api/notifications.go",
		Old: "rendering: make(chan struct{}, MaxRenderingRequests),", New: "rendering: make(chan struct{}, 1000),",
		Package: "./internal/api", Test: "TestConcurrentRequestsRenderInBoundedMemory",
	},
	{
		Name: "api: a request's database work has a deadline (review F5)", File: "internal/api/notifications.go",
		Old: "return context.WithTimeout(r.Context(), s.deadline)", New: "return context.WithCancel(r.Context())",
		Package: "./internal/api", Test: "TestARequestEndsAtItsDeadlineWhenPostgresStalls",
	},
	{
		Name: "api: an answer is encoded before its status (review F4)", File: "internal/api/health.go",
		Old: "\tif err != nil {\n\t\twriteError(w, http.StatusInternalServerError, apiError{Code: \"internal\", Message: \"the answer could not be encoded\"})", New: "\tif false {\n\t\twriteError(w, http.StatusInternalServerError, apiError{Code: \"internal\", Message: \"the answer could not be encoded\"})",
		Package: "./internal/api", Test: "TestAnAnswerThatCannotBeEncodedIsA500",
	},
	{
		Name: "api: a repeat answers the stored row with its history (review F6)", File: "internal/api/notifications.go",
		Old: "if n, err = s.store.Get(ctx, n.ID); err != nil {", New: "if err = error(nil); err != nil {",
		Package: "./internal/api", Test: "TestARepeatedCreateAnswersWithItsAttemptHistory",
	},
	{
		Name: "api: an Idempotency-Key header on a single create is refused (review F9)", File: "internal/api/notifications.go",
		Old: "if r.Header.Get(\"Idempotency-Key\") != \"\" {\n\t\t_, drain := body(w, r, MaxBodyBytes)", New: "if false {\n\t\t_, drain := body(w, r, MaxBodyBytes)",
		Package: "./internal/api", Test: "TestAnIdempotencyKeyHeaderOnASingleCreateIsRefused",
	},
	{
		Name: "api: the bearer scheme in any case", File: "internal/api/notifications.go",
		Old: "strings.EqualFold(auth[:7], \"Bearer \")", New: "auth[:7] == \"Bearer \"",
		Package: "./internal/api", Test: "TestAPIKeysAreRequiredWhenConfigured",
	},
	{
		Name: "provider: a send's error does not quote the URL (session 6's review, F2)", File: "internal/provider/provider.go",
		Old: "r.Err = withoutURL(err)", New: "r.Err = err",
		Package: "./internal/provider", Test: "TestASendErrorDoesNotQuoteTheURL",
	},
	{
		Name: "config: API keys of at least 16 characters", File: "internal/config/config.go",
		Old: "if len(k) < MinAPIKeyLen {", New: "if false && len(k) < MinAPIKeyLen {",
		Package: "./internal/config", Test: "TestAPIKeysMustBeAtLeast16Characters",
	},

	// Session 7: status events, WebSocket, metrics, tracing and alerts (architecture §9, §10).
	{
		Name: "events: Publish never waits for Redis (\u00a79)", File: "internal/events/redis.go",
		Old: "func (p *Redis) Publish(_ context.Context, e Event) {\n\tselect {", New: "func (p *Redis) Publish(ctx context.Context, e Event) {\n\t_ = p.c.Publish(ctx, Channel, e.Encode()).Err()\n\tselect {",
		Package: "./internal/events", Test: "TestPublishNeverBlocksWhenRedisHangs",
	},
	{
		Name: "events: a full buffer drops the event and counts it", File: "internal/events/redis.go",
		Old: "\t\tp.o.Metrics.Dropped.WithLabelValues(\"buffer_full\").Inc()\n", New: "",
		Package: "./internal/events", Test: "TestPublishNeverBlocksWhenRedisHangs",
	},
	{
		Name: "events: one pipeline is bounded by PublishTimeout", File: "internal/events/redis.go",
		Old: "ctx, cancel := context.WithTimeout(ctx, PublishTimeout)", New: "ctx, cancel := context.WithCancel(ctx)",
		Package: "./internal/events", Test: "TestPublishNeverBlocksWhenRedisHangs",
	},
	{
		Name: "events: Close sends what is buffered", File: "internal/events/redis.go",
		Old: "\t\t\tp.drain(batch)\n", New: "\t\t\t_ = batch\n",
		Package: "./internal/events", Test: "TestThePublisherSendsWhatIsBufferedWhenClosed",
	},
	{
		Name: "events: an event over 1 KiB is refused", File: "internal/events/events.go",
		Old: "\tif len(p) > MaxEventBytes {", New: "\tif false {",
		Package: "./internal/events", Test: "TestAMalformedOrOversizedEventIsDropped",
	},
	{
		Name: "events: an event needs a known status", File: "internal/events/events.go",
		Old: "\tif err != nil || w.ID == uuid.Nil {", New: "\tif err != nil && false || w.ID == uuid.Nil {",
		Package: "./internal/events", Test: "TestAMalformedOrOversizedEventIsDropped",
	},
	{
		Name: "events: an event needs a notification id", File: "internal/events/events.go",
		Old: "\tif err != nil || w.ID == uuid.Nil {", New: "\tif err != nil {",
		Package: "./internal/events", Test: "TestAMalformedOrOversizedEventIsDropped",
	},
	{
		Name: "events: a silent subscription is pinged once, then replaced", File: "internal/events/redis.go",
		Old: "if ctx.Err() == nil && timedOut(err) && !pinged {", New: "if ctx.Err() == nil && timedOut(err) && (!pinged || true) {",
		Package: "./internal/events", Test: "TestTheSubscriptionComesBackAfterAnOutage",
	},
	{
		Name: "events: the subscription comes back after an error", File: "internal/events/redis.go",
		Old: "\t\tif ctx.Err() != nil {\n\t\t\treturn\n\t\t}\n\t\tif subscribed {", New: "\t\treturn\n\t\tif subscribed {",
		Package: "./internal/events", Test: "TestTheSubscriptionComesBackAfterRedisDropsIt",
	},
	{
		Name: "events: a cancelled subscription returns at once", File: "internal/events/redis.go",
		Old: "stop := context.AfterFunc(ctx, func() { _ = ps.Close() })", New: "stop := func() bool { return false }",
		Package: "./internal/events", Test: "TestEventsReachSubscribersThroughRedis",
	},
	{
		Name: "dispatch: handle publishes processing (\u00a79)", File: "internal/dispatch/dispatch.go",
		Old: "\td.o.Events.Publish(ctx, events.Event{ID: row.ID, BatchID: row.BatchID, Status: domain.Processing})\n", New: "",
		Package: "./internal/dispatch", Test: "TestEveryStatusChangeOfTheWorkerIsPublished",
	},
	{
		Name: "dispatch: every attempt is counted by its outcome", File: "internal/dispatch/dispatch.go",
		Old: "\td.o.Metrics.Attempts.WithLabelValues(ch, outcomeLabel(v.Outcome)).Inc()\n", New: "",
		Package: "./internal/dispatch", Test: "TestAttemptsAndResultsAreCountedOnce",
	},
	{
		Name: "dispatch: an outcome is a label without spaces", File: "internal/dispatch/dispatch.go",
		Old: "func outcomeLabel(outcome string) string { return strings.ReplaceAll(outcome, \" \", \"_\") }", New: "func outcomeLabel(outcome string) string { return strings.ToLower(outcome) }",
		Package: "./internal/dispatch", Test: "TestAttemptsAndResultsAreCountedOnce",
	},
	{
		Name: "dispatch: what became of every attempt is counted", File: "internal/dispatch/dispatch.go",
		Old: "\t\t\td.o.Metrics.Finished.WithLabelValues(ch, finished).Inc()\n", New: "\t\t\t_ = finished\n",
		Package: "./internal/dispatch", Test: "TestAttemptsAndResultsAreCountedOnce",
	},
	{
		Name: "dispatch: a stale result is counted as stale", File: "internal/dispatch/dispatch.go",
		Old: "\t\t\t\tfinished = \"stale\"\n", New: "",
		Package: "./internal/dispatch", Test: "TestAStaleResultIsCountedAsStale",
	},
	{
		Name: "dispatch: a result that is not stored is counted", File: "internal/dispatch/dispatch.go",
		Old: "\t\t\td.o.Metrics.Finished.WithLabelValues(ch, \"not_stored\").Inc()\n", New: "",
		Package: "./internal/dispatch", Test: "TestResultWriteFailuresAreCountedAndLoggedSparsely",
	},
	{
		Name: "dispatch: a failed result write is counted", File: "internal/dispatch/dispatch.go",
		Old: "\t\td.o.Metrics.Errors.WithLabelValues(\"result_write\").Inc()\n", New: "",
		Package: "./internal/dispatch", Test: "TestResultWriteFailuresAreCountedAndLoggedSparsely",
	},
	{
		Name: "dispatch: a failed result write is logged sparsely (session 5's open item)", File: "internal/dispatch/dispatch.go",
		Old: "\t\td.writes.Log(context.Background(), d.o.Log, slog.LevelWarn, \"result writes failing; retrying\", \"channel\", ch, \"error\", err)", New: "\t\tlog.WarnContext(ctx, \"result writes failing; retrying\", \"error\", err)",
		Package: "./internal/dispatch", Test: "TestResultWriteFailuresAreCountedAndLoggedSparsely",
	},
	{
		Name: "dispatch: the wait at the gate is measured", File: "internal/dispatch/dispatch.go",
		Old: "\td.o.Metrics.GateWait.WithLabelValues(ch).Observe(gate.ended.Sub(gate.began).Seconds())\n", New: "",
		Package: "./internal/dispatch", Test: "TestTheGateWaitAndProviderLatencyAreMeasured",
	},
	{
		Name: "dispatch: the provider's latency is measured from the permit", File: "internal/dispatch/dispatch.go",
		Old: "\t\td.o.Metrics.ProviderLatency.WithLabelValues(ch).Observe(time.Since(gate.ended).Seconds())\n", New: "",
		Package: "./internal/dispatch", Test: "TestTheGateWaitAndProviderLatencyAreMeasured",
	},
	{
		Name: "dispatch: the breaker's state is read when scraped", File: "internal/dispatch/dispatch.go",
		Old: "\to.Metrics.Watch(o.Channel, func() int { return int(d.breaker.State()) }, func() int { return len(d.slots) })\n", New: "",
		Package: "./internal/dispatch", Test: "TestTheBreakerStateIsExportedWhenScraped",
	},
	{
		Name: "dispatch: a failed claim or rate-limit read is counted", File: "internal/dispatch/dispatch.go",
		Old: "\td.o.Metrics.Errors.WithLabelValues(op).Inc()\n", New: "",
		Package: "./internal/dispatch", Test: "TestFailingDependenciesAreCountedAndLoggedSparsely",
	},
	{
		Name: "dispatch: a failed claim or rate-limit read is logged sparsely", File: "internal/dispatch/dispatch.go",
		Old: "\tevery.Log(ctx, d.o.Log, slog.LevelWarn, msg, \"channel\", d.o.Channel, \"error\", err)", New: "\td.o.Log.WarnContext(ctx, msg, \"channel\", d.o.Channel, \"error\", err)",
		Package: "./internal/dispatch", Test: "TestFailingDependenciesAreCountedAndLoggedSparsely",
	},
	{
		Name: "dispatch: the reaper counts the rows it returns", File: "internal/dispatch/reaper.go",
		Old: "\t\t\tr.Metrics.Reaped.WithLabelValues(string(c.Status)).Inc()\n", New: "",
		Package: "./internal/dispatch", Test: "TestTheReaperCountsTheRowsItReturns",
	},
	{
		Name: "dispatch: the reaper's failure is counted and logged sparsely", File: "internal/dispatch/reaper.go",
		Old: "\t\t\tr.Metrics.Errors.WithLabelValues(\"reap\").Inc()\n\t\t\tfailures.Log(ctx, r.Log, slog.LevelError, \"reap failed\", \"error\", err)", New: "\t\t\tr.Log.ErrorContext(ctx, \"reap failed\", \"error\", err)",
		Package: "./internal/dispatch", Test: "TestFailingDependenciesAreCountedAndLoggedSparsely",
	},
	{
		Name: "dispatch: the reaper logs a row with its correlation id (\u00a710)", File: "internal/dispatch/reaper.go",
		Old: "rowCtx := observability.WithCorrelationID(ctx, c.CorrelationID)", New: "rowCtx := ctx",
		Package: "./internal/dispatch", Test: "TestEveryWorkerLineAboutANotificationCarriesItsCorrelationID",
	},
	{
		Name: "dispatch: the delivery span joins the stored trace (\u00a710)", File: "internal/dispatch/dispatch.go",
		Old: "ctx, span := d.tracer.Start(observability.WithTraceparent(ctx, row.Traceparent), \"deliver\",", New: "ctx, span := d.tracer.Start(ctx, \"deliver\",",
		Package: "./internal/systemtest", Test: "TestTheDeliverySpanJoinsTheAPIRequestsTrace",
	},
	{
		Name: "observability: Every holds back a line within its period", File: "internal/observability/every.go",
		Old: "if !e.last.IsZero() && now.Sub(e.last) < e.period {", New: "if false {",
		Package: "./internal/observability", Test: "TestEveryWritesAtMostOneLinePerPeriod",
	},
	{
		Name: "observability: Every says how many lines it held back", File: "internal/observability/every.go",
		Old: "\t\targs = append(args[:len(args):len(args)], \"suppressed\", skipped)\n", New: "\t\t_ = skipped\n",
		Package: "./internal/observability", Test: "TestEveryWritesAtMostOneLinePerPeriod",
	},
	{
		Name: "observability: without an endpoint tracing is the no-op provider", File: "internal/observability/tracing.go",
		Old: "\tif endpoint == \"\" {\n\t\treturn noop.NewTracerProvider()", New: "\tif false {\n\t\treturn noop.NewTracerProvider()",
		Package: "./internal/observability", Test: "TestTracingIsOffWithoutAnEndpoint",
	},
	{
		Name: "observability: a stored traceparent is read back", File: "internal/observability/tracing.go",
		Old: "\treturn propagator.Extract(ctx, propagation.MapCarrier{\"traceparent\": traceparent})", New: "\treturn ctx",
		Package: "./internal/observability", Test: "TestTracingIsOffWithoutAnEndpoint",
	},
	{
		Name: "api: an instance holds at most MaxConnections WebSockets (\u00a79)", File: "internal/api/ws.go",
		Old: "if h.open.Add(1) > int64(h.o.MaxConnections) {", New: "if h.open.Add(1) > int64(h.o.MaxConnections)+1000 {",
		Package: "./internal/api", Test: "TestWebSocketConnectionsAreBoundedPerInstance",
	},
	{
		Name: "api: a client whose queue is full is let go", File: "internal/api/ws.go",
		Old: "\tdefault:\n\t\th.letGoLocked(f, \"slow\")", New: "\tdefault:\n\t\t_ = f",
		Package: "./internal/api", Test: "TestASlowWebSocketClientIsDisconnectedAndMemoryStaysBounded",
	},
	{
		Name: "api: a client's queue is bounded", File: "internal/api/ws.go",
		Old: "queue: make(chan events.Event, h.o.Queue)", New: "queue: make(chan events.Event, 2_000_000)",
		Package: "./internal/api", Test: "TestASlowWebSocketClientIsDisconnectedAndMemoryStaysBounded",
	},
	{
		Name: "api: a write to a client has a deadline", File: "internal/api/ws.go",
		Old: "wctx, wcancel := context.WithTimeout(ctx, o.WriteTimeout)", New: "wctx, wcancel := context.WithCancel(ctx)",
		Package: "./internal/api", Test: "TestAWriteToAClientThatStopsReadingEndsAtItsDeadline",
	},
	{
		Name: "api: a data message from the client ends the connection", File: "internal/api/ws.go",
		Old: "\t\t\tif data {\n", New: "\t\t\tif data && false {\n",
		Package: "./internal/api", Test: "TestAWebSocketClientMustNotSendDataMessages",
	},
	{
		Name: "api: a peer must answer a ping within the pong timeout", File: "internal/api/ws.go",
		Old: "pctx, pcancel := context.WithTimeout(ctx, o.PongTimeout)", New: "pctx, pcancel := context.WithCancel(ctx)",
		Package: "./internal/api", Test: "TestAWebSocketClientThatDoesNotAnswerPingsIsClosed",
	},
	{
		Name: "api: the server pings every interval", File: "internal/api/ws.go",
		Old: "ping := time.NewTicker(o.PingInterval)", New: "ping := time.NewTicker(time.Hour)",
		Package: "./internal/api", Test: "TestAWebSocketClientThatDoesNotAnswerPingsIsClosed",
	},
	{
		Name: "api: a client names at most so many ids", File: "internal/api/ws.go",
		Old: "\t\tif len(values) > most {", New: "\t\tif false {",
		Package: "./internal/api", Test: "TestWebSocketFiltersAreBounded",
	},
	{
		Name: "api: a client names at least one id", File: "internal/api/ws.go",
		Old: "\tif len(ids)+len(batches) == 0 {", New: "\tif false {",
		Package: "./internal/api", Test: "TestWebSocketFiltersAreBounded",
	},
	{
		Name: "api: a WebSocket takes no other parameter", File: "internal/api/ws.go",
		Old: "\t\tif name != notificationIDParam && name != batchIDParam {", New: "\t\tif false {",
		Package: "./internal/api", Test: "TestWebSocketFiltersAreBounded",
	},
	{
		Name: "api: a request that is not an upgrade is a 400 in the error form", File: "internal/api/ws.go",
		Old: "\tif !isUpgrade(r) {", New: "\tif false {",
		Package: "./internal/api", Test: "TestWebSocketFiltersAreBounded",
	},
	{
		Name: "api: a page of another origin is refused", File: "internal/api/ws.go",
		Old: "\tif !sameOrigin(r) {", New: "\tif false {",
		Package: "./internal/api", Test: "TestOpenAPIMatchesTheServer",
	},
	{
		Name: "api: shutdown closes a WebSocket with 1001", File: "internal/api/ws.go",
		Old: "\t\t\tif f.reason == \"shutdown\" {", New: "\t\t\tif false {",
		Package: "./internal/api", Test: "TestTheHubClosesEveryWebSocketOnShutdown",
	},
	{
		Name: "api: a hub that shut down refuses new clients", File: "internal/api/ws.go",
		Old: "\tif h.closed {\n\t\treturn nil, false\n\t}", New: "\tif false {\n\t\treturn nil, false\n\t}",
		Package: "./internal/api", Test: "TestTheHubClosesEveryWebSocketOnShutdown",
	},
	{
		Name: "api: an event reaches the followers of its batch", File: "internal/api/ws.go",
		Old: "\tfor f := range h.byBatch[e.BatchID] {", New: "\tfor f := range h.byBatch[uuid.Nil] {",
		Package: "./internal/api", Test: "TestAWebSocketClientGetsTheCurrentStateThenEveryChange",
	},
	{
		Name: "api: the current state is sent first", File: "internal/api/ws.go",
		Old: "\tfor _, m := range first {", New: "\tfor _, m := range first[:0] {",
		Package: "./internal/api", Test: "TestAWebSocketClientGetsTheCurrentStateThenEveryChange",
	},
	{
		Name: "api: a made-up method is counted as other (\u00a710)", File: "internal/api/metrics.go",
		Old: "\tif !slices.Contains(observability.HTTPMethods, method) {", New: "\tif false && !slices.Contains(observability.HTTPMethods, method) {",
		Package: "./internal/api", Test: "TestMetricLabelsHaveBoundedValues",
	},
	{
		Name: "api: a path without a route is counted as unmatched", File: "internal/api/metrics.go",
		Old: "\tif route == \"\" {", New: "\tif false {",
		Package: "./internal/api", Test: "TestMetricLabelsHaveBoundedValues",
	},
	{
		Name: "api: a figure from Postgres is read once per 5 s", File: "internal/api/metrics.go",
		Old: "if c.at.IsZero() || time.Since(c.at) >= FiguresTTL {", New: "if true {",
		Package: "./internal/api", Test: "TestTheMetricsEndpointIsComputedOncePerFiveSeconds",
	},
	{
		Name: "api: the window is one of four", File: "internal/api/metrics.go",
		Old: "\tif !ok {\n\t\twriteError(w, http.StatusBadRequest, fieldError(\"window\", \"must be one of 1m, 5m, 15m and 1h\"))", New: "\tif false && !ok {\n\t\twriteError(w, http.StatusBadRequest, fieldError(\"window\", \"must be one of 1m, 5m, 15m and 1h\"))",
		Package: "./internal/api", Test: "TestTheMetricsEndpointReportsTheRecentWindow",
	},
	{
		Name: "api: the window is given once", File: "internal/api/metrics.go",
		Old: "\t\tif len(vs) != 1 {", New: "\t\tif false {",
		Package: "./internal/api", Test: "TestTheMetricsEndpointReportsTheRecentWindow",
	},
	{
		Name: "store: the queue's depth counts its four statuses only", File: "internal/store/stats.go",
		Old: "WHERE status IN ('scheduled', 'pending', 'retrying', 'processing')\nGROUP BY channel, status", New: "GROUP BY channel, status",
		Package: "./internal/api", Test: "TestTheMetricsEndpointReportsTheRecentWindow",
	},
	{
		Name: "store: the oldest due row is due", File: "internal/store/stats.go",
		Old: "      AND ready_at <= now()\n    ORDER BY ready_at\n    LIMIT 1) o ON true", New: "    ORDER BY ready_at\n    LIMIT 1) o ON true",
		Package: "./internal/api", Test: "TestTheMetricsEndpointReportsTheRecentWindow",
	},
	{
		Name: "store: the attempts are those of the window", File: "internal/store/stats.go",
		Old: "WHERE a.started_at >= now() - $1::bigint * interval '1 microsecond'\nGROUP BY n.channel, a.outcome", New: "WHERE true\nGROUP BY n.channel, a.outcome",
		Package: "./internal/api", Test: "TestTheMetricsEndpointReportsTheRecentWindow",
	},
	{
		Name: "store: the delivery latency is of the attempts that delivered", File: "internal/store/stats.go",
		Old: "FILTER (WHERE a.outcome = 'sent')", New: "FILTER (WHERE true)",
		Package: "./internal/api", Test: "TestTheMetricsEndpointReportsTheRecentWindow",
	},
	{
		Name: "migration: the attempts' BRIN index on started_at", File: "internal/store/migrations/00005_attempts_started_at.sql",
		Old: "CREATE INDEX delivery_attempts_started_at ON delivery_attempts USING brin (started_at);", New: "SELECT 1;",
		Package: "./internal/store", Test: "TestTheWindowsAttemptsAreReadThroughTheirIndex",
	},
	{
		Name: "store: a single create stores its traceparent", File: "internal/store/notifications.go",
		Old: "\t\tobservability.Traceparent(ctx))", New: "\t\t\"\")",
		Package: "./internal/systemtest", Test: "TestTheDeliverySpanJoinsTheAPIRequestsTrace",
	},
	{
		Name: "store: a batch stores its traceparent", File: "internal/store/notifications.go",
		Old: "scheduled, observability.Traceparent(ctx)); err != nil {", New: "scheduled, \"\"); err != nil {",
		Package: "./internal/systemtest", Test: "TestTheDeliverySpanJoinsTheAPIRequestsTrace",
	},
	{
		Name: "api: a request's span is the child of its traceparent header", File: "internal/api/health.go",
		Old: "ctx, span := tracer.Start(observability.FromHeaders(ctx, propagation.HeaderCarrier(r.Header)), r.Method,", New: "ctx, span := tracer.Start(observability.FromHeaders(ctx, propagation.HeaderCarrier(nil)), r.Method,",
		Package: "./internal/systemtest", Test: "TestTheDeliverySpanJoinsTheAPIRequestsTrace",
	},
	{
		Name: "api: a request's span is named after its route", File: "internal/api/health.go",
		Old: "\t\tspan.SetName(route)\n", New: "",
		Package: "./internal/systemtest", Test: "TestTheDeliverySpanJoinsTheAPIRequestsTrace",
	},
	{
		Name: "api: a WebSocket upgrade reaches the connection", File: "internal/api/health.go",
		Old: "func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }", New: "func (s *statusWriter) Unwrap() http.ResponseWriter { return nil }",
		Package: "./internal/api", Test: "TestAWebSocketOutlivesTheServersRequestTimeouts",
	},
	{
		Name: "alertcheck: a rule needs a case in which it fires (\u00a710)", File: "tools/alertcheck/main.go",
		Old: "\t\t\tif len(c.ExpAlerts) > 0 {", New: "\t\t\tif true {",
		Package: "./tools/alertcheck", Test: "TestEveryAlertRuleHasAFiringTest",
	},
	{
		Name: "alertcheck: a metric no scrape exports is found", File: "tools/alertcheck/main.go",
		Old: "\tif !ok {\n\t\treturn []string{fmt.Sprintf(\"%s: no binary exports the metric %s\", alert, s.metric)}", New: "\tif false && !ok {\n\t\treturn []string{fmt.Sprintf(\"%s: no binary exports the metric %s\", alert, s.metric)}",
		Package: "./tools/alertcheck", Test: "TestTheCheckRefusesWhatNoBinaryExports",
	},
	{
		Name: "alertcheck: a matched label value must be exported", File: "tools/alertcheck/main.go",
		Old: "\t\tcase m.op == \"=\" && !values[m.value]:", New: "\t\tcase false:",
		Package: "./tools/alertcheck", Test: "TestTheCheckRefusesWhatNoBinaryExports",
	},
	{
		Name: "alertcheck: each value of a regex must be exported", File: "tools/alertcheck/main.go",
		Old: "\t\t\t\tif !values[v] {", New: "\t\t\t\tif false {",
		Package: "./tools/alertcheck", Test: "TestTheCheckRefusesWhatNoBinaryExports",
	},
	{
		Name: "alertcheck: a grouping label must be on the metric", File: "tools/alertcheck/main.go",
		Old: "\t\tif _, ok := labels[l]; !ok {", New: "\t\tif false {",
		Package: "./tools/alertcheck", Test: "TestTheCheckRefusesWhatNoBinaryExports",
	},
	{
		Name: "redisclient: the client's own lines are logged sparsely", File: "internal/redisclient/redisclient.go",
		Old: "\tl.every.Log(context.Background(), l.log, slog.LevelWarn, \"redis client\", \"message\", fmt.Sprintf(format, v...))", New: "\tl.log.Warn(\"redis client\", \"message\", fmt.Sprintf(format, v...))",
		Package: "./internal/redisclient", Test: "TestTheRedisClientsOwnLinesAreLoggedSparsely",
	},
	{
		Name: "api: a failed readiness check is logged sparsely", File: "internal/api/health.go",
		Old: "failures[name].Log(r.Context(), log, slog.LevelWarn, \"readiness check failed\", \"dependency\", name, \"error\", err)", New: "log.WarnContext(r.Context(), \"readiness check failed\", \"dependency\", name, \"error\", err)",
		Package: "./internal/api", Test: "TestAFailingReadinessCheckIsLoggedSparsely",
	},
}
