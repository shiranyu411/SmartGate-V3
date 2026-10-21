"""Private HTTP/SSH helpers restricted to the local OpenWrt rehearsal VM."""
import json
import os
from pathlib import Path
import subprocess
import urllib.request


class RehearsalAPI:
    def __init__(self, token_file):
        self.repo = Path(__file__).resolve().parents[2]
        self.token = Path(token_file).read_text().strip()
        self.base = "http://127.0.0.1:13000"

    def start_remote(self, command):
        guard = (
            '. scripts/cutover/common.sh; load_target; '
            'test "$TARGET_KIND" = rehearsal && test "$TARGET" = devvm && '
            'test "$SSH_HOST" = 127.0.0.1 && test "$SSH_PORT" = 2222 || '
            '{ echo "local rehearsal target required" >&2; exit 2; }; remote_sh "$1"'
        )
        return subprocess.Popen(
            ["bash", "-c", guard, "rehearsal", command], cwd=self.repo,
            env={**os.environ, "TARGET": "devvm"}, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True,
        )

    def remote(self, command):
        process = self.start_remote(command)
        output, error = process.communicate()
        if process.returncode:
            raise RuntimeError("rehearsal command failed: " + error[-300:])
        return output

    def api(self, path, data=None):
        body = None if data is None else json.dumps(data).encode()
        request = urllib.request.Request(
            self.base + path, data=body,
            headers={"Authorization": "Bearer " + self.token,
                     "Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=70) as response:
            result = json.load(response)
        if not result.get("ok"):
            raise RuntimeError("API rejected rehearsal action")
        return result["data"]

    def save(self, path, value):
        body = json.dumps(value, ensure_ascii=False)
        self.remote(
            "umask 077\ncat > '" + path + ".next' <<'SMARTGATE_VM_FIXTURE'\n" +
            body + "\nSMARTGATE_VM_FIXTURE\nmv '" + path + ".next' '" + path + "'"
        )
