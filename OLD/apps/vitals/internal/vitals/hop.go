package vitals

// Temperatuur kan een app niet zelf meten: de sensor is van het board en het
// pad loopt via HOP's heartbeat naar de leader (board.TempMilliC → agent →
// /v1/agents). Vitals bevraagt dus de agent-API — elke agent proxyt de
// leader-API door, dus HOP_ADDR mag gewoon naar de eigen node (10.100.0.1:8080)
// wijzen. With HOP_KEY, requests use HOP HMAC authentication; without a key,
// the API is queried unsigned for explicitly open nodes. De handtekening moet byte-voor-byte gelijk zijn
// aan hop/pkg/httputil.Sign (zelfde schema als hop-os-surf's app/hopapi).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sync"
	"time"

	"github.com/xinix00/lean/leanhttp"
)

// agent is het /v1/agents-antwoord, alleen de velden die vitals gebruikt.
// TempMilliC is de heetste sensor van de node, in milli-°C (0 = geen sensor).
type agent struct {
	ID         string `json:"id"`
	Endpoint   string `json:"endpoint"`
	TempMilliC int    `json:"temp_milli_c"`
}

// tempCache dempt het API-verkeer: de burn-test vraagt elke paar seconden en
// de state-poll van de pagina ook — één echte call per 5s is genoeg (de
// sensorwaarde vernieuwt toch per heartbeat).
type tempCache struct {
	mu sync.Mutex
	at time.Time
	v  int
}

// get geeft de nodetemperatuur in milli-°C; 0 = onbekend (geen
// sensor, of de API antwoordt niet — voor een meetinstrument is "geen cijfer"
// beter dan een oud cijfer dat vers oogt).
func (t *tempCache) get(cfg Config) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.at) < 5*time.Second {
		return t.v
	}
	t.at = time.Now()
	t.v = fetchTemp(cfg)
	return t.v
}

// fetchTemp reads the selected node's temperature. With no host configured,
// only a single-node response can be attributed safely.
func fetchTemp(cfg Config) int {
	const path = "/v1/agents"
	var headers leanhttp.Header
	if cfg.HopKey != "" {
		headers = leanhttp.Header{"X-Hop-Auth": sign(cfg.HopKey, "GET", path, nil)}
	}
	resp, err := leanhttp.Do(leanhttp.Call{
		URL:     "http://" + cfg.HopAddr + path,
		Header:  headers,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != leanhttp.StatusOK {
		return 0
	}
	var agents []agent
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return 0
	}
	for _, a := range agents {
		endpoint, err := url.Parse(a.Endpoint)
		if err == nil && cfg.Host != "" && endpoint.Hostname() == cfg.Host {
			return a.TempMilliC
		}
	}
	if cfg.Host == "" && len(agents) == 1 {
		return agents[0].TempMilliC
	}
	return 0
}

// sign bouwt HOP's request-handtekening: HMAC-SHA256 over
// METHOD\nPATH\nhex(sha256(body)). Identiek aan hop/pkg/httputil.Sign.
func sign(key, method, path string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(method + "\n" + path + "\n" + hex.EncodeToString(sum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}
