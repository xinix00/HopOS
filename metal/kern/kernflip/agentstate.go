package kernflip

import "fmt"

// De agent-state-haak. HopOS' kern kent HOP's agent niet (die woont in
// xinix00/hop en wordt door de main bedraad), dus de main geeft hier een
// functie af waarmee de flip de state kan uitlezen vlak vóór de sprong.
//
// Waarom een haak en geen import: kern/kernflip is kernlaag en mag niet van de
// orchestrator afhangen — dan zou elke board-probe en elke test die de flip
// aanraakt de hele agent meeslepen. Niet gezet = geen agent op deze node (de
// demo-mains), en dan gaat er simpelweg geen agent-state mee.
var agentSnapshot func() ([]byte, error)

// UseAgentState registreert de snapshot-functie (cmd/hopos, via
// agentboot.Options.OnSnapshot).
func UseAgentState(snap func() ([]byte, error)) { agentSnapshot = snap }

// snapshotAgent refuses a flip that would lose a configured agent's owners.
func snapshotAgent() ([]byte, error) {
	if agentSnapshot == nil {
		return nil, nil
	}
	b, err := agentSnapshot()
	if err != nil {
		return nil, fmt.Errorf("snapshot agent: %w", err)
	}
	if len(b) > maxAgentState {
		return nil, fmt.Errorf("agent state is %d bytes, limit %d", len(b), maxAgentState)
	}
	return b, nil
}
