#!/usr/bin/env python3
"""meet: de meetronde van docs/measurements.md als één programma.

    python3 tools/meet.py                    alle borden, de O6N als tegenpartij
    python3 tools/meet.py --only pi4,pi5     een paar borden
    python3 tools/meet.py --skip-wire        zonder de metingen over de draad
    python3 tools/meet.py --peer 192.168.1.122 --write
    python3 tools/meet.py --write --no-run   de fragmenten van --out schrijven, niet meten
    python3 tools/meet.py --selftest         de toets van --write

Zes fasen, met de tijd van elk in de eindregel:
1. inrichting: per bord drie peilingen van /v1/agents (*In rust*), dan
   `meet-vitals` op elk bord en de tegenpartij: `bench-serve` op :9000 op een
   big core en vitals op :8090 (voor het bord dat zelf de tegenpartij is:
   `meet-serve` op :9100 en vitals op de M4);
2. vitals: alle borden tegelijk, elke test één run (`secs=5`);
3. de verbindingscyclus en de storm over de draad: serieel;
4. bench in de node: alle borden tegelijk (pull 400 MB, ping warm en koud);
5. bench over de draad: serieel (pull en push, ping);
6. opruimen: elke `meet-*`-job weg; `bench-serve` blijft op zijn big core.

Er wordt nooit blind geslapen: een stap wacht op zijn marker op de console
(poort 5555, één lezer per bord die de hele ronde openblijft en na een
flip opnieuw verbindt), met een tijdslimiet en een foutregel. Alleen de
peilingen van *In rust* (10 s uit elkaar) en het idle-venster van vitals
zijn wachttijd, want daar is de wachttijd de meting.

Per bord komt een fragment in target/meet/<bord>.md (rij | cel | run, de
vorm van de meetagenten) met de console ernaast; `--write` zet de cellen
kaal in de kolom *Nu* van docs/measurements.md en in *Hoogste v3* waar Nu
beter is (meer is beter, behalve de rijen in LAAG). Python met
alleen de standaardbibliotheek: dit is regie (HTTP, JSON, tekst), het meten
zelf gebeurt op de nodes.
"""
import argparse
import json
import re
import socket
import sys
import textwrap
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MIB = 1 << 20

# De borden, in de volgorde van measurements.md. `shares` is cpu_shares van
# vitals (0: geen cpu_shares, de LicheeRV weigert ze), `tags` geldt voor
# vitals en bench, `mem` het geheugen per app waar 128 MiB (vitals) en 64 MiB
# (bench) niet passen, `disk` de maat van vitals disk in MB (0: geen schijf),
# `slow` rekt de tijdslimieten.
BOARDS = {
    'o6n': dict(name='O6N', ip='192.168.1.205', section='Orion O6N', shares=4096,
                tags={'core-class': 'big'}, big=True, disk=256),
    'altra': dict(name='Altra', ip='192.168.1.178', section='Ampere Altra', shares=4096, disk=64),
    'pi5': dict(name='Pi 5', ip='192.168.1.207', section='Raspberry Pi 5', shares=2048),
    'pi4': dict(name='Pi 4', ip='192.168.1.40', section='Raspberry Pi 4', shares=2048),
    'radxa': dict(name='Radxa', ip='192.168.1.241', section='Radxa Zero 3W', shares=2048),
    'licheerv': dict(name='LicheeRV', ip='192.168.1.150', section='LicheeRV Nano', shares=0,
                     arch='riscv64', mem={'vitals': 32 * MIB, 'bench': 16 * MIB},
                     tags={'sharegroup': 'system'},
                     wire=100 * MIB, slow=3),
    'm4': dict(name='M4', ip='192.168.1.122', section='Mac mini M4', shares=2048, big=True, disk=64),
}

# De rijen: sleutel, tabel, rijnaam, en de voorvoegsels waaronder oudere
# fragmenten en de borden die de rij net anders schrijven hem kennen.
ROWS = [
    ('cpu', 'Vitals', 'cpu, Msteps/s'),
    ('smp', 'Vitals', 'smp, speedup'),
    ('burn', 'Vitals', 'burn begin → eind, Msteps/s / max °C', 'burn'),
    ('membw', 'Vitals', 'membw copy / triad, GB/s'),
    ('memlat', 'Vitals', 'memlat 32 KB / 2 MB / 8 MB, ns', 'memlat'),
    ('alloc', 'Vitals', 'alloc, allocs/s'),
    ('storm', 'Vitals', 'storm, conn/s (p99 ms)'),
    ('rtt', 'Vitals', 'rtt naar de kern p50 / p99, µs'),
    ('timer', 'Vitals', 'timer 1 ms, overslaap p50 / p99, µs'),
    ('idle', 'Vitals', 'idle, wekken/s'),
    ('disk', 'Vitals', 'disk schrijven / lezen / 4 KiB schrijven, MB/s', 'disk'),
    ('in', 'Netwerk', 'De node in, MB/s'),
    ('out', 'Netwerk', 'De node uit, MB/s'),
    ('wrtt', 'Netwerk', 'rtt over de draad p50 / p99 / koud, µs', 'rtt over de draad'),
    ('cycle', 'Netwerk', 'Verbindingscyclus p50, ms'),
    ('wstorm', 'Netwerk', 'Storm over de draad, conn/s (p99 ms)', 'Storm over de draad'),
    ('hairpin', 'Netwerk', 'Storm, hairpin naar zichzelf, conn/s (p99 ms)'),
    ('a2a', 'In de node', 'App naar app, MB/s'),
    ('nrtt', 'In de node', 'rtt warm p50 / p99, µs'),
    ('ncold', 'In de node', 'rtt koud p50, µs'),
    ('kcpu', 'In rust', 'Kern-cpu, %', 'Kern-cpu'),
    ('kmem', 'In rust', 'Kern-geheugen gebruikt / totaal, MiB', 'Kern-geheugen'),
    ('hop', 'In rust', 'Hop geheugen, MiB / cpu, %', 'Hop'),
    ('temp', 'In rust', 'Temperatuur, °C', 'Temperatuur'),
]
# Hier is minder beter (tijd, wekken, warmte, verbruik); elders meer.
LAAG = {'memlat', 'rtt', 'timer', 'idle', 'wrtt', 'cycle', 'nrtt', 'ncold', 'kcpu', 'kmem', 'hop', 'temp'}
# De O6N schrijft zijn vitals-disk in *Opslag*, met de 4 KiB apart.
OVERRIDE = {'o6n': {'disk': ('Opslag', 'App-opslag via vitals disk, schrijven / lezen, MB/s'),
                    'disk4k': ('Opslag', '4 KiB schrijven via de app, MB/s')}}

T0 = time.time()
PRINT = threading.Lock()


def say(who, msg):
    with PRINT:
        print(f'[{time.time() - T0:6.1f}] {who:8} {msg}', flush=True)


class Fout(Exception):
    """Een stap die niet lukte; de reden komt in het fragment."""


class Skip(Exception):
    """Een test die zichzelf oversloeg (smp met één core, disk zonder schijf)."""


def http(method, url, body=None, wait=60, timeout=20):
    """Eén verzoek; een bord dat niet antwoordt, krijgt `wait` s de tijd."""
    end = time.time() + wait
    while True:
        req = urllib.request.Request(url, data=body, method=method,
                                     headers={'Content-Type': 'application/json'} if body else {})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()
        except OSError as e:
            if time.time() > end:
                raise Fout(f'{method} {url}: {e}') from None
            time.sleep(2)


def rx(pattern):
    return re.compile(pattern.encode(), re.M)


class Console:
    """De console van één bord, de hele ronde open. Na een wegval (een flip)
    verbindt hij opnieuw en plakt alleen aan wat na de oude staart komt."""

    def __init__(self, ip):
        self.ip, self.buf, self.cv = ip, bytearray(), threading.Condition()
        self.ready, self.stop = threading.Event(), False
        threading.Thread(target=self._run, daemon=True).start()

    def _add(self, d):
        with self.cv:
            self.buf += d
            self.cv.notify_all()

    def _run(self):
        while not self.stop:
            try:
                with socket.create_connection((self.ip, 5555), timeout=5) as s:
                    s.settimeout(0.5)
                    tail, pend = bytes(self.buf[-256:]), bytearray()
                    live = not tail
                    while not self.stop:
                        try:
                            d = s.recv(65536)
                        except TimeoutError:
                            # Een stilte: de replay van de ring is binnen.
                            if not live:
                                self._add(b'\n-- meet: console opnieuw verbonden --\n' + pend)
                                live = True
                            self.ready.set()
                            continue
                        if not d:
                            break
                        if live:
                            self._add(d)
                            continue
                        pend += d
                        i = pend.find(tail)
                        if i >= 0:
                            self._add(pend[i + len(tail):])
                            live = True
            except OSError:
                pass
            time.sleep(1)

    def mark(self):
        with self.cv:
            return len(self.buf)

    def wait(self, pattern, since, timeout, what):
        """De eerste regel na `since` die op `pattern` past, of een Fout."""
        end = time.time() + timeout
        with self.cv:
            while True:
                m = pattern.search(self.buf, since)
                if m:
                    return m
                left = end - time.time()
                if left <= 0:
                    raise Fout(f'{what}: niets binnen {timeout:.0f} s')
                self.cv.wait(min(left, 1))

    def stamp(self):
        with self.cv:
            s = re.findall(rb'HOPOS_BOOT gen=\d+ stamp=(\S+)', self.buf)
        return s[-1].decode() if s else None


class Node:
    def __init__(self, key, cfg, art):
        self.key, self.cfg, self.art = key, cfg, art
        self.ip, self.name = cfg['ip'], cfg['name']
        self.arch = cfg.get('arch', 'arm64')
        self.api = f'http://{self.ip}:9080'
        self.slow = cfg.get('slow', 1)
        self.con = Console(self.ip)
        self.slots, self.cells, self.vitals_up = {}, {}, 0.0
        self.serve_ip = self.stamp = None
        self.ok = False
        self.before = []

    def where(self):
        """Waar de bench-jobs staan, als dat het getal verklaart."""
        t = self.cfg.get('tags', {})
        return 'big cores, ' if t.get('core-class') == 'big' else f'`{t["sharegroup"]}`, ' if 'sharegroup' in t else ''

    def elf(self, app):
        return f'{self.art}/{app}-{self.arch}.elf'

    def spec(self, app, mem, shares, env=None, ports=None, tags=None):
        s = {'driver': 'hop', 'artifacts': [{'url': self.elf(app)}], 'memory_limit': self.cfg.get('mem', {}).get(app, mem)}
        if self.cfg['shares'] and shares:
            s['cpu_shares'] = shares
        tags = self.cfg.get('tags') if tags is None else tags
        if tags:
            s['tags'] = tags
        if env:
            s['env'] = env
        if ports:
            s['ports'] = ports
        return s

    def jobs(self):
        st, b = http('GET', f'{self.api}/v1/jobs')
        if st != 200:
            raise Fout(f'GET /v1/jobs: {st}')
        return json.loads(b or b'[]')

    def delete(self, name):
        """Altijd een DELETE; stond hij er, dan wachten tot zijn slot stopt
        (een job die geweigerd werd, heeft geen slot: dan niet)."""
        listed = any(j.get('name') == name for j in self.jobs())
        slot = self.slots.pop(name, 0)
        since = self.con.mark()
        st, b = http('DELETE', f'{self.api}/v1/jobs/{name}')
        if st not in (200, 202, 204):
            raise Fout(f'DELETE {name}: {st} {b[:120]!r}')
        if listed and slot is not None:
            pat = rf'^slot {slot}: stopped' if slot else r'HOPOS_SLOT_STOPPED'
            try:
                self.con.wait(rx(pat), since, 20 * self.slow, f'{name} stopt')
            except Fout:
                if slot:
                    raise

    def place(self, name, spec, up=None, timeout=30):
        """DELETE, POST, en wachten op HOP_JOB_PLACED en de startmarker
        `up` (een patroon met {slot}). Geeft de match en de plek op de
        console van vlak voor de POST."""
        self.delete(name)
        since = self.con.mark()
        st, b = http('POST', f'{self.api}/v1/jobs', json.dumps({'name': name, **spec}).encode())
        if st not in (200, 201, 202):
            raise Fout(f'POST {name}: {st} {b[:160]!r}')
        n = re.escape(name)
        m = self.con.wait(rx(rf'job {n} (?:task \w+ placed HOP_JOB_PLACED slot=(\d+)|(refused[^\n]*|task \w+ did not start[^\n]*))'),
                          since, timeout * self.slow, f'{name} geplaatst')
        if m.group(2):
            self.slots[name] = None
            raise Fout(f'{name}: {m.group(2).decode(errors="replace").strip()[:160]}')
        slot = int(m.group(1))
        self.slots[name] = slot
        if up:
            m = self.con.wait(rx(up.format(slot=slot)), since, timeout * self.slow, f'{name} op slot {slot}')
        return m, since

    def bench(self, role, peer, nbytes=None, timeout=90):
        """Eén bench-client (pull, push, ping) als `meet-bench`; zijn markerregel(s)."""
        env = {'BENCH': role, 'BENCH_PEER': peer}
        if nbytes:
            env['BENCH_BYTES'] = str(nbytes)
        _, since = self.place('meet-bench', self.spec('bench', 64 * MIB, 1024, env))
        s = self.slots['meet-bench']
        mark = {'pull': r'BENCH_PULL bytes=[^\n]*HOPOS_BENCH_PULL', 'push': r'BENCH_PUSH [^\n]*HOPOS_BENCH_PUSH',
                'ping': r'BENCH_COLD [^\n]*HOPOS_BENCH_COLD'}[role]
        m = self.con.wait(rx(rf'^slot {s}: (?:({mark})|([^\n]*HOPOS_BENCH_FAIL[^\n]*))'), since,
                          timeout * self.slow, f'bench {role} op slot {s}')
        if m.group(2):
            raise Fout(m.group(2).decode(errors='replace').strip()[:160])
        out = [kv(m.group(1))]
        if role == 'ping':
            out.insert(0, kv(self.con.wait(rx(rf'^slot {s}: (BENCH_RTT [^\n]*)HOPOS_BENCH_RTT'), since, 5, 'BENCH_RTT').group(1)))
        return out

    def refused(self):
        """Werd de laatste `meet-bench` geweigerd (geen capaciteit)? Dan
        geldt dat ook voor de volgende rol."""
        return self.slots.get('meet-bench', 0) is None

    def vitals(self, test, timeout=60, **q):
        """Eén vitals-test; wacht op zijn regel op de console en leest dan de
        meetwaarden (alle cijfers) uit /api/state."""
        base = f'http://{self.ip}:8090'
        since = self.con.mark()
        st, b = http('GET', f'{base}/api/run?' + urllib.parse.urlencode({'test': test, **q}))
        if st != 200:
            raise Fout(f'{test}: /api/run {st} {b[:120]!r}')
        s = self.slots['meet-vitals']
        self.con.wait(rx(rf'^slot {s}: vitals: {test} (?:done in|failed after|skipped)'), since,
                      timeout * self.slow, f'vitals {test}')
        st, b = http('GET', f'{base}/api/state')
        r = json.loads(b)['results'][test]
        if 'error' in r:
            raise Fout(f'{test}: {r["error"]}')
        if 'skipped' in r:
            raise Skip(r['skipped'])
        return {x['name']: x['value'] for x in r['metrics']}


def kv(line):
    """`a=1 b=23us c="x"` als dict met getallen waar het kan."""
    out = {}
    for k, v in re.findall(r'(\w+)=("[^"]*"|\S+)', line.decode(errors='replace')):
        v = v.strip('"')
        try:
            out[k] = float(v.removesuffix('us'))
        except ValueError:
            out[k] = v
    return out


def nl(v, dec=None):
    """Een getal zoals measurements.md het schrijft: decimale komma, een punt
    voor duizendtallen vanaf 10.000, drie cijfers waar het geen geheel is."""
    if dec is None:
        dec = 0 if abs(v) >= 100 else 1 if abs(v) >= 10 else 2
    whole, _, frac = f'{v:.{dec}f}'.partition('.')
    if abs(v) >= 10000:
        whole = f'{int(whole):,}'.replace(',', '.')
    return whole + (',' + frac if frac else '')


def span(vals, dec=None):
    a, b = nl(min(vals), dec), nl(max(vals), dec)
    return a if a == b else f'{a}–{b}'


def cores(n):
    return f'{n:.0f} core' + ('' if n == 1 else 's')


def mbps(v):
    return nl(v, 1 if v < 1000 else 0)


def sleutel(bord, row):
    """De sleutel van een rij: exact op de naam, anders op een voorvoegsel."""
    for key, (_, name) in OVERRIDE.get(bord, {}).items():
        if row == name:
            return key
    for key, _, name, *pre in ROWS:
        if row == name:
            return key
    return next((r[0] for r in ROWS for p in r[3:] if row.startswith(p)), None)


def kaal(t):
    """De cel zonder tekst: getallen, `/`, `→` en een getal tussen haakjes.
    Peer, cores en grootte staan in de Opzet-regel."""
    m = re.match(r'([\d.,–]+) \(naar [^,]+, p99 ([\d.,–]+)( ms)?[^)]*\)(?:, koud ([\d.,–]+))?', t)
    if m:
        return f'{m[1]} / {m[2]} / {m[4]}' if m[4] else f'{m[1]} ({m[2]})'
    m = re.match(r'([\d.,–]+) van ([\d.,]+) MiB', t)
    if m:
        return f'{m[1]} / {m[2]}'
    m = re.match(r'([\d–]+) %, ([\d.,–]+) MiB', t)
    if m:
        return f'{m[2]} / {m[1]}'
    k4 = re.search(r'4 KiB ([\d.,]+)', t)
    t = re.sub(r'\(p99 ([\d.,–]+) ms\)', r'(\1)', t)
    t = re.sub(r'met `[^`]*`', '', t)
    t = re.sub(r'\s*\([^)]*[A-Za-z][^)]*\)', '', t)
    t = re.sub(r', (?=\d)', ' / ', t.replace(' %', '').replace(' °C', ''))
    return ' '.join(t.split()) + (f' / {k4[1]}' if k4 else '')


def getallen(t):
    return [float(x.replace('.', '').replace(',', '.')) for x in re.findall(r'\d[\d.]*(?:,\d+)?', t)]


def beter(key, nu, was):
    """Is `nu` beter dan `was`? Het eerste getal telt, de volgende bij gelijkspel."""
    a, b = getallen(nu), getallen(was)
    if not a:
        return False
    return not b or (a < b if key in LAAG else a > b)


def schrijf(lines, bord, stamp, date, opzet, cells):
    """Eén bord in de regels van docs/measurements.md: *Nu* overschrijven,
    *Hoogste v3* bijwerken waar Nu beter is, de Opzet-regel vervangen. Een
    rij die er niet is komt achteraan in zijn tabel."""
    cfg = BOARDS[bord]
    try:
        start = lines.index(f'## {cfg["section"]}')
    except ValueError:
        print(f'meet: geen sectie {cfg["section"]}')
        return
    end = next((i for i in range(start + 1, len(lines)) if lines[i].startswith('## ')), len(lines))
    for key, text in cells.items():
        table, row = OVERRIDE.get(bord, {}).get(key, next(((r[1], r[2]) for r in ROWS if r[0] == key), (None, None)))
        if not row:
            continue
        nu = kaal(text)
        top = f'{nu} ({stamp}, {date})'
        at = next((i for i in range(start, end) if lines[i].startswith('| ')
                   and sleutel(bord, lines[i].split(' | ')[0][2:]) == key), None)
        if at is None:
            head = next((i for i in range(start, end) if lines[i] == f'**{table}**'), None)
            if head is None:
                print(f'meet: {bord} {key}: geen tabel {table}')
                continue
            at = head + 1
            while at + 1 < end and (not lines[at].startswith('|') or lines[at + 1].startswith('|')):
                at += 1
            lines.insert(at + 1, f'| {row} | {nu} | {top} | |')
            end += 1
            continue
        c = [x.strip() for x in lines[at].strip()[1:-1].split('|')]
        c[1] = nu
        m = re.match(r'(.*) \((?:[^()]*, )?\d\d-\d\d\)$', c[2])
        if not m or beter(key, nu, m[1]):
            c[2] = top
        lines[at] = ('| ' + ' | '.join(c) + ' |').replace('|  |', '| |')
    o = next((i for i in range(start, end) if lines[i].startswith('Opzet:')), None)
    if o is not None:
        e = o
        while e < end and lines[e].strip():
            e += 1
        lines[o:e] = textwrap.wrap(f'Opzet: {opzet}', 76, break_long_words=False, break_on_hyphens=False)


def fragment(path, stamps):
    """Een fragment van --out terug: (bord, stempel, opzet, cellen)."""
    bord = path.stem
    stamp, opzet, cells = stamps.get(bord), None, {}
    for line in path.read_text().split('\n'):
        if line.startswith('Opzet: '):
            opzet = line[7:]
        elif line.startswith('- ') and line.count(' | ') == 2:
            row, text, run = line[2:].split(' | ')
            key = sleutel(bord, row)
            if key and not text.startswith('FOUT'):
                cells[key] = text
                stamp = stamp or (run.split()[0] if run.split()[0] != '?' else None)
    if opzet and stamp:
        opzet = re.sub(r'^(None|\?),', f'{stamp},', opzet)
    return bord, stamp, opzet, cells


def write(boards, date):
    """Zet de borden in docs/measurements.md (zie 'Zo schrijf je deze pagina')."""
    doc = ROOT / 'docs/measurements.md'
    lines = doc.read_text().split('\n')
    for bord, stamp, opzet, cells in boards:
        if not stamp:
            print(f'meet: {bord} niet geschreven: geen stempel (--stamp {bord}=...)')
            continue
        schrijf(lines, bord, stamp, date, opzet, cells)
    doc.write_text('\n'.join(lines))
    print(f'meet: {doc} bijgewerkt')


def selftest():
    """--write op een klein voorbeeld: nieuw record, geen record, een nieuwe
    rij, de Opzet-regel, en twee keer schrijven verandert niets."""
    doc = textwrap.dedent('''\
        ## Raspberry Pi 4

        Opzet: P90, 03-10, tools/meet.

        **Vitals**

        | Meting | Nu | Hoogste v3 | v2 |
        | --- | --- | --- | --- |
        | cpu, Msteps/s | 248 | 249 (P2g, 03-10) | |
        | rtt naar de kern p50 / p99, µs | 448 / 1853 | 322 / 5312 (P40, 03-10) | 600 |

        **Netwerk**

        | Meting | Nu | Hoogste v3 | v2 |
        | --- | --- | --- | --- |
        | De node in, MB/s | 58,4 | 71,9 (P40, 03-10) | 6,6 |

        ## Raspberry Pi 5
        ''').split('\n')
    cells = {'cpu': '250', 'rtt': '449 / 2672', 'in': '62,3 (bench pull van de O6N, 256 MiB)',
             'wrtt': '1213 (naar de O6N, p99 1223), koud 1221', 'kcpu': '2 %'}
    opzet = '3.0.10, 04-10, tools/meet. Vitals met 2 cores, 128 MiB.'
    schrijf(doc, 'pi4', '3.0.10', '04-10', opzet, cells)
    once = list(doc)
    schrijf(doc, 'pi4', '3.0.10', '04-10', opzet, cells)
    want = {
        '| cpu, Msteps/s | 250 | 250 (3.0.10, 04-10) | |',
        '| rtt naar de kern p50 / p99, µs | 449 / 2672 | 322 / 5312 (P40, 03-10) | 600 |',
        '| De node in, MB/s | 62,3 | 71,9 (P40, 03-10) | 6,6 |',
        '| rtt over de draad p50 / p99 / koud, µs | 1213 / 1223 / 1221 | 1213 / 1223 / 1221 (3.0.10, 04-10) | |',
        'Opzet: 3.0.10, 04-10, tools/meet. Vitals met 2 cores, 128 MiB.',
    }
    missing = want - set(doc)
    assert not missing, missing
    assert doc == once, 'niet idempotent'
    assert doc.index('| De node in, MB/s | 62,3 | 71,9 (P40, 03-10) | 6,6 |') + 1 == doc.index(
        '| rtt over de draad p50 / p99 / koud, µs | 1213 / 1223 / 1221 | 1213 / 1223 / 1221 (3.0.10, 04-10) | |')
    assert not any(l.startswith('| Kern-cpu') for l in doc)  # geen tabel In rust: niet geschreven
    assert kaal('2990 → 2990, 47,0 °C (4 cores, 10 s)') == '2990 → 2990 / 47,0'
    assert kaal('1337 / 2196 (64 MB; 4 KiB 77,6)') == '1337 / 2196 / 77,6'
    assert kaal('4905 (naar de M4, p99 2,85 ms)') == '4905 (2,85)'
    assert kaal('31,8–31,9 van 224 MiB (14,2 %)') == '31,8–31,9 / 224'
    assert kaal('1 %, 0,45–0,46 MiB, core 0') == '0,45–0,46 / 1'
    assert kaal('752 met `core-class: big`') == '752'
    assert kaal('9971 (0,86)') == '9971 (0,86)' and kaal('844 (p99 19,6 ms)') == '844 (19,6)'
    assert beter('cpu', '10.354', '9.999') and not beter('timer', '48 / 92', '48 / 71')
    print('meet: selftest groen')


class Round:
    def __init__(self, a):
        self.a = a
        self.date = a.date or time.strftime('%d-%m')
        keys = a.only.split(',') if a.only else list(BOARDS)
        for k in keys:
            if k not in BOARDS:
                sys.exit(f'meet: onbekend bord {k} (ken: {", ".join(BOARDS)})')
        byip = {c['ip']: k for k, c in BOARDS.items()}
        self.peer_key = byip.get(a.peer)
        self.counter_key = 'o6n' if self.peer_key == 'm4' else 'm4'
        need = set(keys)
        if not a.skip_wire:
            need.add(self.peer_key or 'peer')
            if self.peer_key in keys:
                need.add(self.counter_key)
        cfgs = dict(BOARDS)
        if not self.peer_key:
            cfgs['peer'] = dict(name=a.peer, ip=a.peer, section='', shares=1024)
            self.peer_key = 'peer'
        self.nodes = {k: Node(k, cfgs[k], a.art.rstrip('/')) for k in cfgs if k in need}
        self.measured = [self.nodes[k] for k in keys]
        self.peer = self.nodes.get(self.peer_key)
        self.counter = self.nodes.get(self.counter_key) if self.peer_key in keys else None
        self.serve_before = None
        self.phases = []

    def the(self, n):
        return f'de {n.name}' if n.cfg['section'] else n.ip

    def target(self, n):
        """De tegenpartij van `n` over de draad: (node, poort van de serve)."""
        return (self.counter, 9100) if n is self.peer else (self.peer, 9000)

    def each(self, nodes, fn):
        """`fn` op elk bord tegelijk; een fout stopt alleen dat bord."""
        def run(n):
            try:
                fn(n)
            except Fout as e:
                say(getattr(n, 'key', 'peer'), f'FOUT {e}')
            except Exception as e:  # een fout in dit programma: dit bord stopt, de rest gaat door
                say(getattr(n, 'key', 'peer'), f'FOUT (meet) {type(e).__name__}: {e}')
        ts = [threading.Thread(target=run, args=(n,)) for n in nodes]
        for t in ts:
            t.start()
        for t in ts:
            t.join()

    def phase(self, name, fn):
        t = time.time()
        say('meet', f'== {name}')
        fn()
        self.phases.append((name, time.time() - t))

    def cell(self, n, key, fn):
        """Draait één meetpunt; de cel is de tekst of de fout."""
        try:
            n.cells[key] = ('ok', fn())
        except Skip as e:
            say(n.key, f'{key}: overgeslagen ({e})')
            return
        except Fout as e:
            n.cells[key] = ('fout', str(e))
        say(n.key, f'{key}: {n.cells[key][1]}')

    # -- 1. inrichting --------------------------------------------------
    def setup(self):
        def pre(n):
            if not n.con.ready.wait(30 * n.slow):
                raise Fout('console 5555 antwoordt niet')
            n.before = n.jobs()
            n.stamp = self.a.stamp.get(n.key) or n.con.stamp()
            n.ok = True
            say(n.key, f'stempel {n.stamp or "? (geef --stamp " + n.key + "=...)"}, jobs '
                + ', '.join(j['name'] for j in n.before))
        self.each(self.nodes.values(), pre)
        self.measured = [n for n in self.measured if n.ok]
        self.each(self.measured, self.rest)
        work = [n for n in self.nodes.values() if n.ok]
        if not self.a.skip_wire and self.peer:
            work.append('serve')

        def place(n):
            if n == 'serve':
                return self.peer_serve()
            m, _ = n.place('meet-vitals', n.spec('vitals', 128 * MIB, n.cfg['shares'], ports={'http': 8090}),
                        up=r'^slot {slot}: vitals [^\n]*?(\d+) core\(s\) HOPOS_VITALS_UP')
            n.vitals_up, n.vitals_cores = time.time(), int(m.group(1))
            say(n.key, f'meet-vitals op slot {n.slots["meet-vitals"]}, {cores(n.vitals_cores)}')
            if n is self.counter:
                self.serve(n)
        self.each(work, place)

    def peer_serve(self):
        """bench-serve op de tegenpartij: :9000, eigen core, big waar die er is."""
        p = self.peer
        old = next((j for j in p.before if j.get('name') == 'bench-serve'), None)
        self.serve_before = old
        tags = {'core-class': 'big'} if p.cfg.get('big') else {}
        if old and old.get('tags', {}) == tags:
            say(p.key, 'bench-serve staat er al zo')
            return
        p.place('bench-serve', p.spec('bench', 64 * MIB, 1024, {'BENCH': 'serve', 'BENCH_PORT': '9000'},
                                      {'bench': 9000}, tags=tags),
                up=r'^slot {slot}: bench: serving [^\n]*HOPOS_BENCH_UP')
        say(p.key, f'bench-serve op slot {p.slots["bench-serve"]} ({tags or "eerste vrije core"})')

    def serve(self, n):
        """`meet-serve` op :9100: de serve van de bench in de node, en op de
        tegenpartij van de peer ook zijn serve over de draad."""
        if n.serve_ip:
            return
        m, _ = n.place('meet-serve', n.spec('bench', 64 * MIB, 1024, {'BENCH': 'serve', 'BENCH_PORT': '9100'},
                                         {'bench': 9100}),
                    up=r'^slot {slot}: bench: serving [^\n]* on tcp ([\d.]+):9100[^\n]*HOPOS_BENCH_UP')
        n.serve_ip = m.group(1).decode()
        say(n.key, f'meet-serve op slot {n.slots["meet-serve"]}, {n.serve_ip}:9100')

    def rest(self, n):
        """*In rust*: drie peilingen van /v1/agents, 10 s uit elkaar."""
        polls = []
        for i in range(3):
            if i:
                time.sleep(10)
            st, b = http('GET', f'{n.api}/v1/agents')
            a = json.loads(b) if st == 200 else []
            a = next((x for x in a if n.ip in x.get('endpoint', '')), a[0] if a else {})
            if 'kern_cpu_percent' not in a:
                say(n.key, f'In rust: geen telemetrie (Hop {a.get("version", "?")})')
                return
            polls.append(a)
        g = lambda k: [p.get(k, 0) for p in polls]  # zonder sensor geen temp_milli_c
        n.cells['kcpu'] = ('ok', span(g('kern_cpu_percent'), 0) + ' %')
        ram = polls[0]['kern_ram_bytes'] / MIB
        mem = [m / MIB for m in g('kern_mem_bytes')]
        n.cells['kmem'] = ('ok', f'{span(mem, 1)} van {nl(ram, 0 if ram == int(ram) else 1)} MiB '
                                 f'({nl(100 * sum(mem) / len(mem) / ram, 1)} %)')
        n.cells['hop'] = ('ok', f'{span(g("hop_cpu_percent"), 0)} %, {span([m / MIB for m in g("hop_mem_bytes")], 2)} MiB, '
                                f'core {polls[0]["hop_core"]}')
        if min(g('temp_milli_c')) > 0:
            n.cells['temp'] = ('ok', span([t / 1000 for t in g('temp_milli_c')], 1) + ' °C')
        say(n.key, 'In rust: ' + '; '.join(n.cells[k][1] for k in ('kcpu', 'kmem', 'hop') if k in n.cells))

    # -- 2. vitals ------------------------------------------------------
    def vitals(self, n):
        if 'meet-vitals' not in n.slots:
            raise Fout('meet-vitals staat er niet')
        c = n.vitals_cores
        note = ' met `core-class: big`' if n.cfg.get('tags', {}).get('core-class') == 'big' else ''
        # Het idle-venster loopt vanaf de start van vitals: 20 s stilte.
        time.sleep(max(0.0, n.vitals_up + 20 - time.time()))

        def idle():
            st, b = http('GET', f'http://{n.ip}:8090/api/state')
            w = json.loads(b)['idle']
            if not w.get('ok'):
                raise Fout('idle: nog geen venster')
            pct = f', {nl(w["idle_pct"], 1)} % idle' if w.get('idle_pct') is not None else ''
            return f'{nl(w["wakes_per_s"], 1)} ({cores(c)}{pct})'
        self.cell(n, 'idle', idle)
        self.cell(n, 'cpu', lambda: nl(n.vitals('cpu', secs=5)['rate']) + note)
        self.cell(n, 'smp', lambda: (lambda m: f'{nl(m["speedup"])} ({cores(m["cores"])})')(n.vitals('smp', 120)))

        def burn():
            m = n.vitals('burn', 90, secs=5)
            t = f', {nl(m["temp_max"], 1)} °C' if 'temp_max' in m else ''
            return f'{nl(m["rate_start"])} → {nl(m["rate_end"])}{t} ({cores(m["cores"])}, 10 s)'
        self.cell(n, 'burn', burn)
        self.cell(n, 'membw', lambda: (lambda m: f'{nl(m["copy"])} / {nl(m["triad"])}')(n.vitals('membw', 90)))

        def memlat():
            m = n.vitals('memlat', 120)
            if 'ns_8m' in m:
                return f'{nl(m["ns_32k"])} / {nl(m["ns_2m"])} / {nl(m["ns_8m"])}'
            return f'{nl(m["ns_32k"])} / {nl(m["ns_2m"])} (geen 8 MB)'
        self.cell(n, 'memlat', memlat)
        self.cell(n, 'alloc', lambda: nl(n.vitals('alloc', secs=5)['allocs_per_s']))
        self.cell(n, 'storm', lambda: (lambda m: f'{nl(m["rate"])} ({nl(m["p99"])})')(n.vitals('storm')))
        self.cell(n, 'hairpin', lambda: (lambda m: f'{nl(m["rate"])} ({nl(m["p99"])})')(
            n.vitals('storm', addr=f'{n.ip}:8090')))

        def rtt():
            m = n.vitals('rtt')
            e = f' ({m["errors"]:.0f} fouten)' if m.get('errors') else ''
            return f'{nl(m["p50"], 0)} / {nl(m["p99"], 0)}{e}'
        self.cell(n, 'rtt', rtt)
        self.cell(n, 'timer', lambda: (lambda m: f'{nl(m["oversleep_1ms_p50"], 0)} / {nl(m["oversleep_1ms_p99"], 0)}')(
            n.vitals('timer')))
        if n.cfg.get('disk'):
            mb = n.cfg['disk']

            def disk():
                m = n.vitals('disk', 180, mb=mb)
                if n.key in OVERRIDE:
                    n.cells['disk4k'] = ('ok', nl(m['write_4k']))
                    return f'{nl(m["write"])} / {nl(m["read"])} ({mb} MB)'
                return f'{nl(m["write"])} / {nl(m["read"])} ({mb} MB; 4 KiB {nl(m["write_4k"])})'
            self.cell(n, 'disk', disk)

    # -- 3. de storm over de draad ------------------------------------
    def wire_vitals(self):
        for n in self.measured:
            t, _ = self.target(n)
            if not t or 'meet-vitals' not in t.slots or 'meet-vitals' not in n.slots:
                say(n.key, 'storm over de draad: geen vitals aan een van beide kanten')
                continue
            addr = f'{t.ip}:8090'
            self.cell(n, 'wstorm', lambda: (lambda m: f'{nl(m["rate"])} (naar {self.the(t)}, p99 {nl(m["p99"])} ms)')(
                n.vitals('storm', addr=addr)))
            self.cell(n, 'cycle', lambda: (lambda m: f'{nl(m["p50"])} (naar {self.the(t)}, vitals storm n=1)')(
                n.vitals('storm', addr=addr, n=1)))

    # -- 4. bench in de node -------------------------------------------
    def in_nodes(self):
        self.each([n for n in self.nodes.values() if 'meet-vitals' in n.slots], lambda n: n.delete('meet-vitals'))
        self.each(self.measured, self.in_node)

    def in_node(self, n):
        self.serve(n)
        peer = f'{n.serve_ip}:9100'
        self.cell(n, 'a2a', lambda: f'{nl(n.bench("pull", peer, 400 * MIB)[0]["MBps"])} ({n.where()}400 MB)')
        try:
            if n.refused():
                raise Fout(n.cells['a2a'][1])
            rtt, cold = n.bench('ping', peer)
            n.cells['nrtt'] = ('ok', f'{nl(rtt["p50"], 0)} / {nl(rtt["p99"], 0)}')
            n.cells['ncold'] = ('ok', nl(cold['p50'], 0))
        except Fout as e:
            n.cells['nrtt'] = n.cells['ncold'] = ('fout', str(e))
        say(n.key, f'ping in de node: {n.cells["nrtt"][1]}, koud {n.cells["ncold"][1]}')
        n.delete('meet-bench')
        if n is not self.counter or self.a.skip_wire:
            n.delete('meet-serve')
            n.serve_ip = None

    # -- 5. bench over de draad ----------------------------------------
    def wire(self):
        for n in self.measured:
            t, port = self.target(n)
            if not t or (port == 9100 and not t.serve_ip):
                say(n.key, 'over de draad: geen serve op de tegenpartij')
                continue
            peer, size = f'{t.ip}:{port}', n.cfg.get('wire', 256 * MIB)
            mib = f'{size // MIB} MiB'
            self.cell(n, 'in', lambda: f'{mbps(n.bench("pull", peer, size)[0]["MBps"])} ({n.where()}bench pull van {self.the(t)}, {mib})')
            if n.refused():
                continue
            self.cell(n, 'out', lambda: f'{mbps(n.bench("push", peer, size)[0]["MBps"])} ({n.where()}bench push naar {self.the(t)}, {mib})')

            def ping():
                rtt, cold = n.bench('ping', peer)
                return f'{nl(rtt["p50"], 0)} (naar {self.the(t)}, p99 {nl(rtt["p99"], 0)}), koud {nl(cold["p50"], 0)}'
            self.cell(n, 'wrtt', ping)
            n.delete('meet-bench')

    # -- 6. opruimen -----------------------------------------------------
    def cleanup(self):
        def clean(n):
            for j in n.jobs():
                if j.get('name', '').startswith('meet-'):
                    n.delete(j['name'])
            if n is self.peer and self.serve_before is None and 'bench-serve' in n.slots:
                n.delete('bench-serve')
            left = [j['name'] for j in n.jobs()]
            say(n.key, 'jobs nu: ' + ', '.join(left))
        self.each(self.nodes.values(), clean)

    def run(self):
        try:
            self.phase('inrichting', self.setup)
            self.phase('vitals', lambda: self.each(self.measured, self.vitals))
            if not self.a.skip_wire:
                self.phase('storm over de draad', self.wire_vitals)
            self.phase('in de node', self.in_nodes)
            if not self.a.skip_wire:
                self.phase('over de draad', self.wire)
        finally:
            self.phase('opruimen', self.cleanup)
        self.report()

    # -- uitvoer ---------------------------------------------------------
    def rows(self, n):
        out = []
        for key, table, row, *prefix in ROWS + [('disk4k', 'Opslag', '')]:
            table, row = OVERRIDE.get(n.key, {}).get(key, (table, row))
            if key in n.cells and row:
                out.append((key, table, row, prefix[0] if prefix else None, *n.cells[key]))
        return out

    def opzet(self, n):
        c = getattr(n, 'vitals_cores', None)
        tags = ', '.join(f'`{k}: {v}`' for k, v in n.cfg.get('tags', {}).items())
        s = f'{n.stamp}, {self.date}, tools/meet.'
        if c:
            s += f' Vitals met {cores(c)}{" (" + tags + ")" if tags else ""}, {n.cfg.get("mem", {}).get("vitals", 128 * MIB) // MIB} MiB.'
        naast = [j['name'] for j in n.before if not j['name'].startswith('meet-')]
        if naast:
            s += ' Naast ' + ', '.join(naast) + '.'
        if not self.a.skip_wire:
            t, port = self.target(n)
            what = 'bench-serve' if port == 9000 else 'meet-serve'
            s += f' Over de draad tegen {self.the(t)} ({what} :{port}, vitals :8090).'
        return s

    def report(self):
        out = Path(self.a.out)
        out.mkdir(parents=True, exist_ok=True)
        for n in self.measured:
            (out / f'{n.key}.console').write_bytes(bytes(n.con.buf))
            run = f'{n.stamp or "?"} {self.date}'
            lines = [f'# {n.name} ({n.ip}), tools/meet {self.date}', '', f'Opzet: {self.opzet(n)}', '']
            for table in dict.fromkeys(r[1] for r in self.rows(n)):
                lines.append(f'## {table}')
                lines += [f'- {row} | {"FOUT: " if st == "fout" else ""}{text} | {run}'
                          for _, tb, row, _, st, text in self.rows(n) if tb == table]
                lines.append('')
            (out / f'{n.key}.md').write_text('\n'.join(lines))
        # De samenvatting: een rij per meetpunt, een kolom per bord.
        short = lambda t: re.sub(r'\s*\([^)]*\)', '', t)[:18]
        keys = [r[0] for r in ROWS + [('disk4k',)] if any(r[0] in n.cells for n in self.measured)]
        print('\n' + f'{"":8} ' + ' '.join(f'{n.name:<18}' for n in self.measured))
        for k in keys:
            cells = [n.cells.get(k, ('', '')) for n in self.measured]
            print(f'{k:8} ' + ' '.join(f'{"FOUT" if st == "fout" else short(t):<18}' for st, t in cells))
        fouten = [(n, k, t) for n in self.measured for k, (st, t) in n.cells.items() if st == 'fout']
        for n, k, t in fouten:
            print(f'FOUT {n.key} {k}: {t}')
        if self.a.write:
            write([(n.key, n.stamp, self.opzet(n), {k: t for k, (st, t) in n.cells.items() if st == 'ok'})
                   for n in self.measured], self.date)
        print(f'\nmeet: fragmenten in {out}/; ' + ', '.join(f'{p} {s:.0f} s' for p, s in self.phases)
              + f'; totaal {time.time() - T0:.0f} s')


def main():
    p = argparse.ArgumentParser(description='De meetronde van docs/measurements.md.')
    p.add_argument('--only', help='borden, met komma\'s: ' + ','.join(BOARDS))
    p.add_argument('--skip-wire', action='store_true', help='zonder de metingen over de draad')
    p.add_argument('--peer', default=BOARDS['o6n']['ip'], help='de tegenpartij over de draad (IP)')
    p.add_argument('--art', default='http://192.168.1.208:8000', help='bron van vitals-<arch>.elf en bench-<arch>.elf')
    p.add_argument('--stamp', action='append', default=[], help='bord=STEMPEL als de console hem niet meer heeft')
    p.add_argument('--date', help='dd-mm in de cellen (standaard vandaag)')
    p.add_argument('--out', default=str(ROOT / 'target/meet'), help='map voor de fragmenten')
    p.add_argument('--write', action='store_true', help='Nu en Hoogste v3 in docs/measurements.md zetten')
    p.add_argument('--no-run', action='store_true', help='niet meten, alleen de fragmenten van --out schrijven')
    p.add_argument('--selftest', action='store_true', help='de toets van --write op een voorbeeld')
    a = p.parse_args()
    a.stamp = dict(s.split('=', 1) for s in a.stamp)
    if a.selftest:
        return selftest()
    if a.no_run:
        keys = a.only.split(',') if a.only else list(BOARDS)
        paths = [Path(a.out) / f'{k}.md' for k in keys if (Path(a.out) / f'{k}.md').exists()]
        return write([fragment(p, a.stamp) for p in paths], a.date or time.strftime('%d-%m'))
    Round(a).run()


if __name__ == '__main__':
    main()
