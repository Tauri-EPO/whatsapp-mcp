"""Resolve Docker's effective endpoint and pin local-only subprocess calls."""

import json
import os
import subprocess


class LocalDocker:
    def __init__(self, cwd=None):
        self.cwd = cwd
        self.env = dict(os.environ)
        context = self.env.pop("DOCKER_CONTEXT", "")
        host = self.env.pop("DOCKER_HOST", "")
        if context or not host:
            command = ["docker", "context", "inspect", *([context] if context else [])]
            host = json.loads(subprocess.check_output(command, env=self.env, text=True))[0]["Endpoints"]["docker"][
                "Host"
            ]
        if not host.startswith(("npipe://", "unix://")):
            raise SystemExit("Refusing a remote Docker daemon")
        self.command = ["docker", "--host", host]

    def run(self, *args, **kwargs):
        env = dict(kwargs.pop("env", self.env))
        env.pop("DOCKER_CONTEXT", None)
        env.pop("DOCKER_HOST", None)
        return subprocess.run([*self.command, *args], cwd=self.cwd, env=env, **kwargs)

    def __call__(self, *args, **kwargs):
        result = self.run(*args, text=True, stdout=subprocess.PIPE, **kwargs)
        result.check_returncode()
        return result.stdout.strip()
