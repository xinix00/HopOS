package vitals

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// rxProgress/rxZeroReads: voortgang van de lopende rx-test (bytes gelezen) en
// het aantal Reads dat (0, nil) teruggaf — een lus die daarop blijft draaien
// is een spin, geen wachten.
var rxProgress, rxZeroReads, txProgress atomic.Int64

// stallWatch logt elke seconde de voortgang van de rx-test en dumpt, zodra er
// vier seconden niets beweegt, alle goroutine-stacks naar het task-log — dat
// is het enige kanaal dat HOP nog leest als de netstack van de app zelf klem
// zit (node→node-hang boven 1 MiB, 06-09). Draait op zijn eigen goroutine,
// dus op een 2-core-app ook als de pomp of de lezer hangt.
func (s *Server) stallWatch(name string, progress *atomic.Int64, done <-chan struct{}) {
	last, since := progress.Load(), time.Now()
	dumped := false
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		cur := progress.Load()
		var c map[string]uint64
		if s.cfg.Counters != nil {
			c = s.cfg.Counters()
		}
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		fmt.Fprintf(&b, "stallwatch %s: %d bytes zero-reads %d goroutines %d", name, cur, rxZeroReads.Load(), runtime.NumGoroutine())
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%d", k, c[k])
		}
		s.cfg.Logf("%s", b.String())
		if cur != last {
			last, since = cur, time.Now()
			continue
		}
		if !dumped && time.Since(since) > 4*time.Second {
			dumped = true
			buf := make([]byte, 256<<10)
			n := runtime.Stack(buf, true)
			s.cfg.Logf("stallwatch: no progress for %.0fs — goroutine dump follows", time.Since(since).Seconds())
			for _, l := range strings.Split(string(buf[:n]), "\n") {
				if l != "" {
					s.cfg.Logf("stack: %s", l)
				}
			}
			s.cfg.Logf("stallwatch: end of dump")
		}
	}
}
