"""WHATSAPP_PUBLIC_URL: the expiry of the certificate clients actually meet.

The probe runs against a real TLS server started in the test process, with
certificates generated on the fly, so both the valid and the expired case are
exercised end to end without a network.
"""

from __future__ import annotations

import _ssl
import datetime
import ipaddress
import socket
import ssl
import threading

import pytest

# Locked (via mcp -> pyjwt[crypto]), so this import is deterministic in CI: if
# it ever stops being installed these tests must fail loudly, not skip away.
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

import endpoint_cert
import main
import whatsapp

HOST = "127.0.0.1"
SANS = x509.SubjectAlternativeName([x509.DNSName("localhost"), x509.IPAddress(ipaddress.ip_address(HOST))])


def _write(path, blob):
    path.write_bytes(blob)
    return str(path)


def _make_chain(tmp_path, not_before, not_after):
    """A tiny CA plus a leaf valid between the given instants. Returns paths."""
    ca_key = ec.generate_private_key(ec.SECP256R1())
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "whatsapp-mcp test CA")])
    now = datetime.datetime.now(datetime.UTC)
    ca = (
        x509.CertificateBuilder()
        .subject_name(ca_name)
        .issuer_name(ca_name)
        .public_key(ca_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(days=365))
        .not_valid_after(now + datetime.timedelta(days=365))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .add_extension(x509.SubjectKeyIdentifier.from_public_key(ca_key.public_key()), critical=False)
        .add_extension(
            x509.KeyUsage(
                digital_signature=False,
                content_commitment=False,
                key_encipherment=False,
                data_encipherment=False,
                key_agreement=False,
                key_cert_sign=True,
                crl_sign=True,
                encipher_only=False,
                decipher_only=False,
            ),
            critical=True,
        )
        .sign(ca_key, hashes.SHA256())
    )
    leaf_key = ec.generate_private_key(ec.SECP256R1())
    leaf = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, HOST)]))
        .issuer_name(ca_name)
        .public_key(leaf_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(not_before)
        .not_valid_after(not_after)
        .add_extension(SANS, critical=False)
        .add_extension(x509.BasicConstraints(ca=False, path_length=None), critical=True)
        # OpenSSL 3 (ssl.create_default_context, strict mode) rejects a chain
        # whose leaf does not point at its issuer.
        .add_extension(
            x509.AuthorityKeyIdentifier.from_issuer_public_key(ca_key.public_key()),
            critical=False,
        )
        .sign(ca_key, hashes.SHA256())
    )
    ca_pem = _write(tmp_path / "ca.pem", ca.public_bytes(serialization.Encoding.PEM))
    cert_pem = _write(tmp_path / "leaf.pem", leaf.public_bytes(serialization.Encoding.PEM))
    key_pem = _write(
        tmp_path / "leaf.key",
        leaf_key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        ),
    )
    return ca_pem, cert_pem, key_pem


class TlsServer:
    """Accepts TLS connections and closes them: a handshake, nothing else."""

    def __init__(self, certfile, keyfile):
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(certfile, keyfile)
        self._context = context
        self._sock = socket.socket()
        self._sock.bind((HOST, 0))
        self._sock.listen(8)
        self.port = self._sock.getsockname()[1]
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def _serve(self):
        while True:
            try:
                conn, _ = self._sock.accept()
            except OSError:
                return
            try:
                self._context.wrap_socket(conn, server_side=True).close()
            except OSError:
                conn.close()

    def close(self):
        self._sock.close()
        self._thread.join(timeout=5)


@pytest.fixture
def tls_server(tmp_path):
    servers = []

    def start(days_from=-1, days_to=30):
        now = datetime.datetime.now(datetime.UTC)
        directory = tmp_path / f"cert{len(servers)}"
        directory.mkdir()
        ca, cert, key = _make_chain(
            directory,
            now + datetime.timedelta(days=days_from),
            now + datetime.timedelta(days=days_to),
        )
        server = TlsServer(cert, key)
        servers.append(server)
        return server, ca

    yield start
    for server in servers:
        server.close()


def _trusting(ca_pem):
    def factory():
        context = ssl.create_default_context(cafile=ca_pem)
        return context

    return factory


@pytest.fixture(autouse=True)
def _clean_cache():
    endpoint_cert.reset_cache()
    yield
    endpoint_cert.reset_cache()


class TestEndpointTarget:
    @pytest.mark.parametrize(
        ("raw", "expected"),
        [
            ("https://host.tail1234.ts.net/mcp", ("host.tail1234.ts.net", 443)),
            ("https://host.tail1234.ts.net:8443/mcp", ("host.tail1234.ts.net", 8443)),
            ("host.tail1234.ts.net", ("host.tail1234.ts.net", 443)),
            ("host.tail1234.ts.net:8443", ("host.tail1234.ts.net", 8443)),
            ("  https://host.ts.net  ", ("host.ts.net", 443)),
            ("https://[::1]:8443/mcp", ("::1", 8443)),
        ],
    )
    def test_accepted_spellings(self, raw, expected):
        assert endpoint_cert.endpoint_target(raw) == expected

    @pytest.mark.parametrize("raw", [None, "", "   "])
    def test_unset_is_none(self, raw):
        assert endpoint_cert.endpoint_target(raw) is None

    def test_plain_http_is_refused(self):
        with pytest.raises(ValueError, match="https"):
            endpoint_cert.endpoint_target("http://host.ts.net/mcp")

    def test_a_url_without_a_host_is_refused(self):
        with pytest.raises(ValueError, match="no host"):
            endpoint_cert.endpoint_target("https:///mcp")

    @pytest.mark.parametrize("raw", ["https://[::1", "https://host.ts.net:notaport"])
    def test_a_malformed_url_names_the_variable(self, raw):
        # Whatever stdlib says about it, the operator must learn which setting.
        with pytest.raises(ValueError, match="WHATSAPP_PUBLIC_URL"):
            endpoint_cert.endpoint_target(raw)


class TestProbe:
    def test_valid_certificate_reports_the_expiry(self, tls_server):
        server, ca = tls_server(days_to=30)

        out = endpoint_cert.probe(HOST, server.port, context_factory=_trusting(ca))

        assert "endpoint_cert_error" not in out
        assert out["endpoint_cert_expires_at"].endswith("Z")
        assert 28 <= out["endpoint_cert_days_left"] <= 30

    def test_expired_certificate_reports_the_error_and_still_the_date(self, tls_server):
        server, ca = tls_server(days_from=-40, days_to=-10)

        out = endpoint_cert.probe(HOST, server.port, context_factory=_trusting(ca))

        assert "expired" in out["endpoint_cert_error"]
        # The date is the point: an operator needs to see how long ago it went.
        assert out["endpoint_cert_expires_at"].endswith("Z")
        assert out["endpoint_cert_days_left"] < 0

    def test_untrusted_chain_is_an_error_with_the_date(self, tls_server):
        server, _ = tls_server(days_to=30)

        # The real default context: the test CA is not in the system store.
        out = endpoint_cert.probe(HOST, server.port)

        assert "certificate verify failed" in out["endpoint_cert_error"]
        assert 28 <= out["endpoint_cert_days_left"] <= 30

    def test_without_the_decoder_the_error_survives_alone(self, tls_server, monkeypatch, caplog):
        server, _ = tls_server(days_from=-40, days_to=-10)
        # A build (or a runtime) that cannot decode an unverified certificate.
        monkeypatch.delattr(_ssl, "_test_decode_cert", raising=False)

        with caplog.at_level("WARNING", logger="whatsapp_mcp"):
            out = endpoint_cert.probe(HOST, server.port)

        assert "endpoint_cert_error" in out
        assert "endpoint_cert_expires_at" not in out
        assert "cannot decode an unverified certificate" in caplog.text

    def test_nothing_listening_is_an_error_without_a_date(self):
        sock = socket.socket()
        sock.bind((HOST, 0))
        port = sock.getsockname()[1]
        sock.close()

        out = endpoint_cert.probe(HOST, port, timeout_s=2)

        assert "cannot reach" in out["endpoint_cert_error"]
        assert "endpoint_cert_expires_at" not in out and "endpoint_cert_days_left" not in out


class TestStatus:
    def test_absent_when_the_url_is_unset(self):
        assert endpoint_cert.status({}) == {}

    def test_bad_url_is_reported_not_raised(self):
        out = endpoint_cert.status({"WHATSAPP_PUBLIC_URL": "http://host.ts.net"})
        assert "https" in out["endpoint_cert_error"]

    def test_result_is_cached_per_host(self):
        calls = []

        def fake(host, port):
            calls.append((host, port))
            return {"endpoint_cert_days_left": 12}

        env = {"WHATSAPP_PUBLIC_URL": "https://host.ts.net/mcp"}
        first = endpoint_cert.status(env, probe_fn=fake)
        second = endpoint_cert.status(env, probe_fn=fake)

        assert first == second == {"endpoint_cert_days_left": 12}
        assert calls == [("host.ts.net", 443)]

        endpoint_cert.reset_cache()
        endpoint_cert.status(env, probe_fn=fake)
        assert len(calls) == 2

    def test_a_failure_is_cached_far_more_briefly_than_a_certificate(self):
        # Renewing the certificate on the host must show up in the next call,
        # not in an hour.
        env = {"WHATSAPP_PUBLIC_URL": "https://host.ts.net"}
        endpoint_cert.status(env, probe_fn=lambda host, port: {"endpoint_cert_error": "expired"})
        expiry_after_failure = endpoint_cert._cache[("host.ts.net", 443)][0]

        endpoint_cert.reset_cache()
        endpoint_cert.status(env, probe_fn=lambda host, port: {"endpoint_cert_days_left": 20})
        expiry_after_success = endpoint_cert._cache[("host.ts.net", 443)][0]

        assert endpoint_cert.ERROR_CACHE_TTL_S < endpoint_cert.CACHE_TTL_S
        assert expiry_after_failure < expiry_after_success

    def test_a_cached_result_cannot_be_mutated_by_a_caller(self):
        env = {"WHATSAPP_PUBLIC_URL": "https://host.ts.net"}
        out = endpoint_cert.status(env, probe_fn=lambda host, port: {"endpoint_cert_days_left": 3})
        out["endpoint_cert_days_left"] = 999

        assert endpoint_cert.status(env, probe_fn=lambda host, port: {})["endpoint_cert_days_left"] == 3


class Resp:
    def __init__(self, status, payload):
        self.status_code = status
        self._payload = payload
        self.text = ""

    def json(self):
        return self._payload


def _healthy(monkeypatch):
    def fake(url, **kwargs):
        if url.endswith("/health"):
            return Resp(200, {"status": "ok", "connected": True, "paired": True})
        return Resp(500, {})

    monkeypatch.setattr(whatsapp.bridge_http, "get", fake)


class TestBridgeStatusIntegration:
    def test_no_fields_without_the_variable(self, monkeypatch):
        _healthy(monkeypatch)
        monkeypatch.delenv("WHATSAPP_PUBLIC_URL", raising=False)

        out = main.bridge_status()

        assert not [key for key in out if key.startswith("endpoint_cert")]

    def test_fields_when_the_variable_is_set(self, monkeypatch):
        _healthy(monkeypatch)
        monkeypatch.setenv("WHATSAPP_PUBLIC_URL", "https://host.ts.net/mcp")
        monkeypatch.setattr(
            endpoint_cert,
            "probe",
            lambda host, port: {"endpoint_cert_expires_at": "2026-12-01T00:00:00Z", "endpoint_cert_days_left": 84},
        )

        out = main.bridge_status()

        assert out["ok"] is True
        assert out["endpoint_cert_days_left"] == 84
        assert out["endpoint_cert_expires_at"] == "2026-12-01T00:00:00Z"

    def test_a_bad_url_is_an_error_field_not_a_failed_call(self, monkeypatch):
        _healthy(monkeypatch)
        monkeypatch.setenv("WHATSAPP_PUBLIC_URL", "http://myserver.tail1234.ts.net/mcp")

        out = main.bridge_status()

        assert out["ok"] is True
        assert "WHATSAPP_PUBLIC_URL" in out["endpoint_cert_error"]
        assert "endpoint_cert_expires_at" not in out

    def test_a_broken_probe_does_not_break_the_status(self, monkeypatch):
        _healthy(monkeypatch)
        monkeypatch.setenv("WHATSAPP_PUBLIC_URL", "https://host.ts.net/mcp")

        def boom(*args, **kwargs):
            raise RuntimeError("probe exploded")

        monkeypatch.setattr(endpoint_cert, "status", boom)

        out = main.bridge_status()

        assert out["ok"] is True and "probe exploded" in out["endpoint_cert_error"]
