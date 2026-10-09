"""Parse the real Compose merge; no substitute YAML parser or Docker daemon."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
DOCKER = shutil.which("docker")
pytestmark = pytest.mark.skipif(DOCKER is None, reason="needs Docker Compose CLI (config only)")


def config(tmp_path: Path, overrides: tuple[str, ...], **values: str) -> dict:
    empty_env = tmp_path / "empty.env"
    empty_env.touch()
    args = [DOCKER or "docker", "compose", "--env-file", str(empty_env), "-f", "docker-compose.yml"]
    for name in overrides:
        args += ["-f", f"docker-compose.{name}.yml"]
    result = subprocess.run(
        [*args, "config", "--format", "json"],
        cwd=ROOT,
        env={
            **os.environ,
            "COMPOSE_PROJECT_NAME": "proxy-test",
            "WHATSAPP_PROXY_ALIAS": "whatsapp-example",
            "WHATSAPP_PROXY_NETWORK": "proxy-test-network",
            "WHATSAPP_OPERATOR_ALIAS": "operator-example",
            "WHATSAPP_OPERATOR_NETWORK": "operator-test-network",
            "WHATSAPP_OUTBOX": "",
            "WHATSAPP_MCP_PORT": "8000",
            "WHATSAPP_MCP_BIND": "127.0.0.1",
            **values,
        },
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout)


@pytest.mark.parametrize("overrides", [(), ("operator",), ("proxy",), ("proxy", "operator"), ("operator", "proxy")])
def test_network_and_volume_boundaries(tmp_path: Path, overrides: tuple[str, ...]) -> None:
    model = config(tmp_path, overrides)
    bridge, mcp = (model["services"][name] for name in ("bridge", "mcp"))
    assert mcp["network_mode"] == "service:bridge"
    assert "default" in bridge.get("networks", {})
    assert not mcp.get("ports")
    for service in (bridge, mcp):
        mounts = {v["target"]: v for v in service["volumes"]}
        assert mounts["/app/store"]["source"] == "whatsapp-store"
        outbox = mounts["/app/outbox"]
        if "proxy" in overrides:
            assert outbox["type"] == "volume" and outbox["source"] == "whatsapp-outbox"
        else:
            assert outbox["type"] == "bind"
    if "proxy" in overrides:
        assert not bridge.get("ports")
        assert bridge["environment"]["WHATSAPP_BRIDGE_BIND"] == "127.0.0.1"
        assert bridge["networks"]["proxy"]["aliases"] == ["whatsapp-example"]
        assert model["networks"]["proxy"]["name"] == "proxy-test-network"
        assert model["networks"]["proxy"]["external"] is True
        assert model["volumes"]["whatsapp-outbox"]["name"] == "proxy-test_whatsapp-outbox"
    else:
        assert bridge["ports"][0]["host_ip"] == "127.0.0.1"
        assert bridge["ports"][0]["published"] == "8000"
    if "operator" in overrides:
        assert bridge["environment"]["WHATSAPP_OPERATOR_BIND"] == "operator-example"
        assert bridge["networks"]["operator"]["aliases"] == ["operator-example"]
    if overrides:
        assert mcp["environment"]["WHATSAPP_MCP_METRICS"] == "false"


def test_proxy_still_allows_explicit_bind_outbox(tmp_path: Path) -> None:
    model = config(tmp_path, ("proxy",), WHATSAPP_OUTBOX=str(tmp_path))
    for service in model["services"].values():
        mounts = {v["target"]: v for v in service["volumes"]}
        assert mounts["/app/outbox"]["type"] == "bind"
        assert Path(mounts["/app/outbox"]["source"]).resolve() == tmp_path.resolve()


def test_proxy_alias_required(tmp_path: Path) -> None:
    with pytest.raises(AssertionError, match="set a unique proxy alias"):
        config(tmp_path, ("proxy",), WHATSAPP_PROXY_ALIAS="")
