"""Local probes must follow Docker context precedence and pin their endpoint."""

import json
import subprocess
from pathlib import Path

import pytest


def load_helper(monkeypatch):
    monkeypatch.syspath_prepend(str(Path(__file__).parents[2] / "scripts"))
    import docker_local

    return docker_local


def test_remote_context_overrides_local_host_and_is_refused(monkeypatch):
    helper = load_helper(monkeypatch)
    monkeypatch.setenv("DOCKER_CONTEXT", "synthetic-remote")
    monkeypatch.setenv("DOCKER_HOST", "unix:///synthetic/local.sock")
    calls = []

    def inspect(command, **kwargs):
        calls.append(command)
        return json.dumps([{"Endpoints": {"docker": {"Host": "ssh://remote.example.test"}}}])

    monkeypatch.setattr(helper.subprocess, "check_output", inspect)
    with pytest.raises(SystemExit, match="remote"):
        helper.LocalDocker()
    assert calls == [["docker", "context", "inspect", "synthetic-remote"]]


def test_local_context_overrides_remote_host_and_calls_are_pinned(monkeypatch):
    helper = load_helper(monkeypatch)
    monkeypatch.setenv("DOCKER_CONTEXT", "synthetic-local")
    monkeypatch.setenv("DOCKER_HOST", "ssh://remote.example.test")
    monkeypatch.setattr(
        helper.subprocess,
        "check_output",
        lambda *args, **kwargs: json.dumps([{"Endpoints": {"docker": {"Host": "unix:///synthetic/local.sock"}}}]),
    )
    docker = helper.LocalDocker()
    calls = []

    def run(command, **kwargs):
        calls.append((command, kwargs["env"]))
        return subprocess.CompletedProcess(command, 0, stdout="synthetic\n")

    monkeypatch.setattr(helper.subprocess, "run", run)
    assert (
        docker("ps", env={"DOCKER_CONTEXT": "changed-remote", "DOCKER_HOST": "ssh://remote.example.test"})
        == "synthetic"
    )
    assert calls == [(["docker", "--host", "unix:///synthetic/local.sock", "ps"], {})]
