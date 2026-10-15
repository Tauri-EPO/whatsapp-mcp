#!/usr/bin/env python3
"""Disposable unpaired TCP + Unix compose smoke; local Docker only, no egress.

Build/tag both images first. Select WHATSAPP_IMAGE_REGISTRY/WHATSAPP_IMAGE_TAG.
Uses the real socket override with separate private volumes instead of host binds so
Docker Desktop's Windows filesystem does not have to support Unix inodes.
"""

import json
import os
import secrets
import subprocess
import tempfile
import time
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


PROBE = r"""
import os,stat,httpx
token=os.environ['WHATSAPP_MCP_TOKEN']
init={'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':'2025-03-26',
      'capabilities':{},'clientInfo':{'name':'synthetic-socket-smoke','version':'0'}}}
info=os.stat('/app/sockets/mcp.sock')
assert stat.S_ISSOCK(info.st_mode) and stat.S_IMODE(info.st_mode)==0o660 and info.st_uid==1000
assert not os.path.exists('/app/sockets/operator.sock')
assert not os.path.exists('/app/operator-sockets/operator.sock')
for uds in (None,'/app/sockets/mcp.sock'):
    with httpx.Client(transport=httpx.HTTPTransport(uds=uds),base_url='http://localhost:8000') as c:
        headers={'Authorization':'Bearer '+token,'Accept':'application/json, text/event-stream'}
        good=c.post('/mcp',json=init,headers=headers)
        assert good.status_code==200 and 'mcp-session-id' in good.headers,good.text
        assert c.post('/mcp',json=init,headers={'Accept':headers['Accept']}).status_code==401
        assert c.post('/mcp',json=init,headers={**headers,'Host':'outside.example.test'}).status_code==421
        sid=good.headers['mcp-session-id']
        hdr={**headers,'mcp-session-id':sid}
        c.post('/mcp',json={'jsonrpc':'2.0','method':'notifications/initialized'},headers=hdr)
        result=c.post('/mcp',json={'jsonrpc':'2.0','id':2,'method':'tools/list'},headers=hdr)
        assert result.status_code==200 and 'list_chats' in result.text
print('PASS TCP + Unix MCP initialize/tools/list, Host/bearer denials, 0660/uid1000; operator socket inaccessible from MCP')
"""

OPERATOR_PROBE = r"""
import io,os,stat,zipfile,httpx
op_token=os.environ['WHATSAPP_OPERATOR_TOKEN']
info=os.stat('/app/sockets/operator.sock')
assert stat.S_ISSOCK(info.st_mode) and stat.S_IMODE(info.st_mode)==0o660 and info.st_uid==1000
for uds in (None,'/app/sockets/operator.sock'):
    with httpx.Client(transport=httpx.HTTPTransport(uds=uds),base_url='http://localhost:8090') as c:
        headers={'Authorization':'Bearer '+op_token}
        assert c.get('/operator/v1/health',headers=headers).status_code==200
        assert c.get('/operator/v1/health').status_code==401
        assert c.get('/operator/v1/health',headers={**headers,'Host':'outside.example.test'}).status_code==403
        with c.stream('GET','/operator/v1/export',headers=headers) as response:
            assert response.status_code==200
            archive=zipfile.ZipFile(io.BytesIO(b''.join(response.iter_bytes())))
            assert archive.namelist()
print('PASS TCP + Unix operator health/export, Host/bearer denials, 0660/uid1000 from separate operator client')
"""


def main():
    from docker_local import LocalDocker

    docker = LocalDocker(ROOT)
    project = "wamcp-socket-" + uuid.uuid4().hex[:10]
    registry = os.environ.get("WHATSAPP_IMAGE_REGISTRY", "ghcr.io/tauri-epo")
    tag = os.environ.get("WHATSAPP_IMAGE_TAG", "latest")
    image = f"{registry}/whatsapp-mcp-server:{tag}"
    env = dict(
        os.environ,
        WHATSAPP_IMAGE_REGISTRY=registry,
        WHATSAPP_IMAGE_TAG=tag,
        WHATSAPP_OPERATOR_TOKEN=secrets.token_hex(32),
        WHATSAPP_OPERATOR_TOKEN_FILE="",
        WHATSAPP_OPERATOR_BIND="127.0.0.1",
        WHATSAPP_OPERATOR_SOCKET="",
        WHATSAPP_MCP_SOCKET="",
        WHATSAPP_OPERATOR_ALLOWED_HOSTS="localhost",
        WHATSAPP_OPERATOR_PORT="8090",
        WHATSAPP_MCP_TOKEN="synthetic-mcp-socket-token-0123456789",
        WHATSAPP_MCP_METRICS="true",
        WHATSAPP_MCP_METRICS_TOKEN="",
        WHATSAPP_MCP_HOST="127.0.0.1",
        WHATSAPP_MCP_ALLOWED_HOSTS="localhost",
        WHATSAPP_MCP_RATE_LIMIT="off",
        WHATSAPP_MCP_TRUSTED_PROXIES="",
        WHATSAPP_PROXY_URL="",
        WHATSAPP_PROXY_URL_FILE="",
        WHATSAPP_MCP_OAUTH_ISSUER="",
        WHATSAPP_ALLOWED_CHATS="",
        WHATSAPP_ALLOW_TOOLS="",
        WHATSAPP_DENY_TOOLS="",
        WHATSAPP_READ_ONLY="false",
        WHATSAPP_BRIDGE_TOKEN="synthetic-bridge-socket-token-0123456789",
        TRANSCRIBE_ON_INGEST="false",
        WHISPER_URL="",
        WHATSAPP_PUBLIC_URL="",
        WHATSAPP_MEDIA_BACKEND="local",
        WHATSAPP_SNAPSHOT_DIR="",
        WHATSAPP_OUTBOX="",
    )
    env.pop("COMPOSE_FILE", None)
    env.pop("COMPOSE_ENV_FILES", None)
    env.pop("DOCKER_CONTEXT", None)
    env["DOCKER_HOST"] = docker.command[-1]
    with tempfile.TemporaryDirectory(prefix="wamcp-socket-", dir=ROOT) as tmp:
        empty = Path(tmp) / "empty.env"
        empty.write_text("", encoding="utf-8")
        override = Path(tmp) / "socket-smoke.yml"
        override.write_text(
            """services:
  socket-init:
    image: """
            + image
            + """
    network_mode: none
    user: '0'
    entrypoint: sh
    volumes:
      - mcp-sockets:/app/mcp-sockets
      - operator-sockets:/app/operator-sockets
      - outbox:/app/outbox
  bridge:
    ports: !reset []
    volumes: !override
      - whatsapp-store:/app/store
      - operator-sockets:/app/sockets
      - outbox:/app/outbox
    environment:
      WHATSAPP_OPERATOR_ALLOWED_HOSTS: localhost
  mcp:
    volumes: !override
      - whatsapp-store:/app/store
      - mcp-sockets:/app/sockets
      - outbox:/app/outbox
networks:
  default:
    internal: true
volumes:
  mcp-sockets:
  operator-sockets:
  outbox:
""",
            encoding="utf-8",
        )
        compose = [
            "compose",
            "-p",
            project,
            "--env-file",
            str(empty),
            "-f",
            "docker-compose.yml",
            "-f",
            "docker-compose.socket.yml",
            "-f",
            str(override),
        ]
        created = False
        try:
            for extra in ([], ["-f", "docker-compose.split.yml"]):
                topology = json.loads(
                    docker(
                        *compose,
                        *extra,
                        "config",
                        "--format",
                        "json",
                        env={
                            **env,
                            "WHATSAPP_AGENT_SUBNET": "172.30.252.0/28",
                            "WHATSAPP_AGENT_BRIDGE_IP": "172.30.252.2",
                            "WHATSAPP_AGENT_MCP_IP": "172.30.252.3",
                        },
                    )
                )
                mcp_mounts = topology["services"]["mcp"]["volumes"]
                assert not any("operator-sockets" in mount["source"] for mount in mcp_mounts), mcp_mounts
            print("PASS operator socket volume excluded from MCP in regular and split Compose configurations")
            # Compose creates only this project's private resources. Initialize
            # its socket volume before uid-1000 listeners start.
            created = True
            docker(
                *compose,
                "run",
                "--rm",
                "--no-deps",
                "--user",
                "0",
                "--entrypoint",
                "sh",
                "socket-init",
                "-c",
                "chown 1000:1000 /app/mcp-sockets /app/operator-sockets /app/outbox; chmod 0770 /app/mcp-sockets /app/operator-sockets",
                env=env,
            )
            docker(*compose, "up", "-d", "--no-build", "bridge", "mcp", env=env)
            for _ in range(60):
                try:
                    docker(
                        *compose,
                        "exec",
                        "-T",
                        "mcp",
                        "python",
                        "-c",
                        "from mcp_config import probe_http_health; probe_http_health()",
                        env=env,
                        stderr=subprocess.DEVNULL,
                    )
                    break
                except subprocess.CalledProcessError:
                    time.sleep(1)
            else:
                raise RuntimeError("Socket services did not start")
            print(
                docker(
                    *compose,
                    "exec",
                    "-T",
                    "mcp",
                    "python",
                    "-c",
                    PROBE,
                    env=env,
                )
            )
            bridge_id = docker(*compose, "ps", "-q", "bridge", env=env)
            print(
                docker(
                    "run",
                    "--rm",
                    "--network",
                    "container:" + bridge_id,
                    "--mount",
                    f"type=volume,source={project}_operator-sockets,target=/app/sockets,readonly",
                    "-e",
                    f"WHATSAPP_OPERATOR_TOKEN={env['WHATSAPP_OPERATOR_TOKEN']}",
                    "--entrypoint",
                    "python",
                    image,
                    "-c",
                    OPERATOR_PROBE,
                    env=env,
                )
            )
            print(
                docker(
                    *compose,
                    "exec",
                    "-T",
                    "bridge",
                    "whatsapp-bridge",
                    "--operator-status",
                    env=env,
                )
            )
            if os.environ.get("CI") == "true" and os.name == "posix":
                mount = docker(
                    "volume",
                    "inspect",
                    project + "_mcp-sockets",
                    "--format",
                    "{{.Mountpoint}}",
                )
                smoke = subprocess.run(
                    [
                        "sudo",
                        "-E",
                        "bash",
                        "scripts/smoke.sh",
                        "--project",
                        project,
                        "--mcp-socket",
                        mount + "/mcp.sock",
                        "--url",
                        "http://localhost",
                    ],
                    cwd=ROOT,
                    env=env,
                    check=False,
                )
                if smoke.returncode != 2:
                    raise RuntimeError(f"smoke.sh over Unix exited {smoke.returncode}; expected 2 (unpaired)")
            print("Images:", image, f"{registry}/whatsapp-mcp-bridge:{tag}")
        finally:
            if created:
                docker(*compose, "down", "-v", "--remove-orphans", env=env)
                if docker(
                    "ps",
                    "-aq",
                    "--filter",
                    f"label=com.docker.compose.project={project}",
                ):
                    raise RuntimeError("Owned socket smoke containers remain")
                if docker(
                    "volume",
                    "ls",
                    "-q",
                    "--filter",
                    f"label=com.docker.compose.project={project}",
                ):
                    raise RuntimeError("Owned socket smoke volumes remain")
                if docker(
                    "network",
                    "ls",
                    "-q",
                    "--filter",
                    f"label=com.docker.compose.project={project}",
                ):
                    raise RuntimeError("Owned socket smoke networks remain")


if __name__ == "__main__":
    main()
