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
		Old: "if err := cfg.Worker.Check(); err != nil {", New: "if err := error(nil); err != nil {",
		Package: "./cmd/worker", Test: "TestWorkerRefusesToStartWithALeaseShorterThanSendTimeoutPlus30s",
	},
	{
		Name: "compose: stop grace period > shutdown timeout", File: "docker-compose.yml",
		Old: "stop_grace_period: 30s", New: "stop_grace_period: 25s",
		Package: "./internal/config", Test: "TestComposeStopGracePeriodExceedsShutdownTimeout",
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
		Name: "api: readyz checks Redis", File: "internal/api/health.go",
		Old:     `checks := map[string]Check{"postgres": h.Postgres, "redis": h.Redis}`,
		New:     `checks := map[string]Check{"postgres": h.Postgres, "redis": func(context.Context) error { return nil }}`,
		Package: "./internal/api", Test: "TestReadyzFailsWhenRedisIsUnreachable",
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
		Old: "MaxHeaderBytes:    MaxHeaderBytes,", New: "MaxHeaderBytes:    0,",
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
}
