module github.com/jeffdafoe/llm-memory-api/go/server

go 1.26.0

toolchain go1.27.3

require (
	github.com/jackc/pgx/v5 v5.12.0
	github.com/pashagolub/pgxmock/v5 v5.2.0
	github.com/pgvector/pgvector-go/pgx v0.4.1
	golang.org/x/text v0.43.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/pgvector/pgvector-go v0.4.1 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/telemetry v0.0.0-20261010193011-f71dc92da45e // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/vuln v1.9.0 // indirect
)

tool golang.org/x/vuln/cmd/govulncheck
