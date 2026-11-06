package agent

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Instructions is an agent's startup instructions and dream mode, as the
// instructions routes and the MCP read_instructions tool read them.
type Instructions struct {
	Startup   *string
	DreamMode *string
}

// LoadInstructions reads the agent's agent_configuration row. found is false
// when the agent has none.
func LoadInstructions(ctx context.Context, conn db.DB, actorID int) (Instructions, bool, error) {
	var in Instructions
	err := conn.QueryRow(ctx, "SELECT startup_instructions, dream_mode FROM agent_configuration WHERE actor_id = $1",
		actorID).Scan(&in.Startup, &in.DreamMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Instructions{}, false, nil
	}
	if err != nil {
		return Instructions{}, false, err
	}
	return in, true, nil
}

// Dreams reports whether the dream mode takes the dream bootstrap and the
// soul note. 'sim-shared' pools many villagers under one agent whose souls
// live in the engine (LLM-199), not in a context/soul note, so like 'none'
// it takes neither.
func (in Instructions) Dreams() bool {
	m := in.DreamMode
	return m != nil && *m != "" && *m != "none" && *m != "sim-shared"
}

// SaveInstructions sets the agent's startup instructions; nil clears them.
func SaveInstructions(ctx context.Context, conn db.DB, actorID int, content *string) error {
	_, err := conn.Exec(ctx, "UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2", content, actorID)
	return err
}
