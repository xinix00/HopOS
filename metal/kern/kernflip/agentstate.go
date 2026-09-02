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

// snapshotAgent leest de agent-state, of geeft niets als er geen agent is.
// Een fout is geen reden om de flip af te blazen: de apps overleven hem toch,
// en de leader herstelt de administratie bij zijn eerste synchronisatie. Wel
// luid, want het verschil tussen "geen agent" en "agent kon niet" is precies
// wat je wil weten als de taken daarna vreemd doen.
func snapshotAgent() []byte {
	if agentSnapshot == nil {
		return nil
	}
	b, err := agentSnapshot()
	if err != nil {
		fmt.Printf("kernflip: could not snapshot the agent state (%v) — flipping without it; the leader will resync\n", err)
		return nil
	}
	if len(b) > maxAgentState {
		fmt.Printf("kernflip: agent state is %d bytes, over the %d-byte handoff limit — flipping without it; the leader will resync\n", len(b), maxAgentState)
		return nil
	}
	return b
}
