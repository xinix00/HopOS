#!/usr/bin/env python3
"""Apply the saved, media-only workload to Derek's standalone O6N."""
import argparse
import json
from pathlib import Path
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--node', default='http://192.168.1.205:8080')
    args = parser.parse_args()
    node = args.node.rstrip('/')
    spec = json.loads((ROOT / 'job-lumen.json').read_text())

    def request(path, method='GET', data=None):
        body = None if data is None else json.dumps(data).encode()
        req = urllib.request.Request(node + path, data=body, method=method,
                                     headers={'Content-Type': 'application/json'})
        with urllib.request.urlopen(req, timeout=30) as response:
            raw = response.read()
            return json.loads(raw) if raw else None

    # DELETE /v1/jobs is cluster-wide. Refuse to affect another node.
    agents = request('/v1/agents')
    if (len(agents) != 1 or agents[0]['endpoint'].rstrip('/') != node
            or agents[0]['id'] != spec['affinity']['node.id']):
        raise SystemExit('Gestopt: dit script vereist alleen de bedoelde o6n in het cluster.')
    with urllib.request.urlopen(urllib.request.Request(
            spec['artifacts'][0]['url'], method='HEAD'), timeout=10) as response:
        if response.status != 200 or int(response.headers.get('Content-Length', 0)) <= 0:
            raise SystemExit('Het Lumen-artifact is niet beschikbaar; niets gestopt.')

    jobs = request('/v1/jobs')
    previous = next((job for job in jobs if job['name'] == spec['name']), None)
    for job in jobs:
        if job['name'] != spec['name']:
            request('/v1/jobs/' + urllib.parse.quote(job['name'], safe=''), 'DELETE')
            print('Verwijderd:', job['name'], flush=True)
    deadline = time.monotonic() + 45
    while True:
        tasks = request('/tasks')
        if not any(t['job_name'] != spec['name'] and t['state'] != 'failed' for t in tasks):
            break
        if time.monotonic() > deadline:
            raise SystemExit('Andere taken stoppen nog; hun resources zijn niet aantoonbaar vrij.')
        time.sleep(1)
    running = any(t['job_name'] == spec['name'] and t['state'] == 'running' for t in tasks)
    if previous != spec and running:
        host = urllib.parse.urlsplit(node).hostname
        portal = f'http://{host}:{spec["ports"]["ui"]}'
        try:
            with urllib.request.urlopen(portal + '/api/state', timeout=12) as response:
                current = json.load(response)
        except (OSError, ValueError) as error:
            raise SystemExit('Update gestopt: actieve back-up niet veilig te controleren.') from error
        if current.get('ingest', {}).get('stage') not in ('idle', 'done', 'error'):
            raise SystemExit('Update uitgesteld: Lumen is nog bezig met een back-up.')
    if previous != spec or not running:
        request('/v1/jobs', 'POST', spec)
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        tasks = request('/tasks')
        if len(tasks) == 1 and tasks[0]['job_name'] == spec['name'] and tasks[0]['state'] == 'running':
            host = urllib.parse.urlsplit(node).hostname
            portal = f'http://{host}:{spec["ports"]["ui"]}'
            try:
                with urllib.request.urlopen(portal + '/api/state', timeout=5) as response:
                    state = json.load(response)
            except (OSError, ValueError):
                time.sleep(1)
                continue
            print('Alleen Lumen draait:', state.get('build'), portal)
            return
        time.sleep(1)
    raise SystemExit('Lumen is nog niet gereed; controleer /tasks en de nodeconsole.')


if __name__ == '__main__':
    main()
