"""Bounded, non-redirecting byte transport shared by validation gates."""

import http.client
import json
import time
import urllib.parse


MAX_REQUEST_BYTES = 65536
MAX_RESPONSE_BYTES = 16 * 1024 * 1024


class HTTPBoundError(Exception):
    """A fixed transport check identifier without upstream response content."""


def request_bytes(endpoint, api_key, method, path, payload, timeout, max_bytes):
    """Return status and bounded bytes; callers own status and JSON semantics.

    Socket budgets shrink against one monotonic deadline between operations.
    read1 prevents a whole-body read from repeatedly renewing its idle timeout.
    HTTPConnection never follows redirects or forwards authorization elsewhere.
    """
    parsed = urllib.parse.urlsplit(endpoint)
    if (parsed.scheme not in ("http", "https") or not parsed.hostname
            or parsed.username is not None or parsed.password is not None
            or parsed.query or parsed.fragment):
        raise ValueError("endpoint must be HTTP(S) without credentials or query")
    parsed.port
    if not path.startswith("/") or path.startswith("//"):
        raise ValueError("request path must be relative to the endpoint")
    if type(max_bytes) is not int or not 1 <= max_bytes <= MAX_RESPONSE_BYTES:
        raise ValueError("response limit is outside supported bounds")
    if not isinstance(timeout, (int, float)) or not 0 < timeout <= 180:
        raise ValueError("timeout is outside supported bounds")
    deadline = time.monotonic() + timeout
    body = None if payload is None else json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    if body is not None and len(body) > MAX_REQUEST_BYTES:
        raise HTTPBoundError("request-too-large")
    headers = {"Accept": "application/json"}
    if body is not None:
        headers["Content-Type"] = "application/json"
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    connection_type = http.client.HTTPSConnection if parsed.scheme == "https" else http.client.HTTPConnection
    connection = connection_type(parsed.hostname, parsed.port, timeout=timeout)

    def remaining():
        budget = deadline - time.monotonic()
        if budget <= 0:
            raise TimeoutError()
        return budget

    try:
        connection.request(method, parsed.path.rstrip("/") + path, body=body, headers=headers)
        # Retain the socket when a Connection: close response detaches it from
        # HTTPConnection. Each read uses the request's remaining time.
        socket = connection.sock
        socket.settimeout(remaining())
        response = connection.getresponse()
        declared = response.getheader("Content-Length")
        if declared is not None:
            try:
                length = int(declared)
            except ValueError:
                raise HTTPBoundError("invalid-content-length") from None
            if length < 0 or length > max_bytes:
                raise HTTPBoundError("response-too-large")
        chunks = bytearray()
        while True:
            socket.settimeout(remaining())
            chunk = response.read1(min(65536, max_bytes + 1 - len(chunks)))
            if not chunk:
                break
            chunks.extend(chunk)
            if len(chunks) > max_bytes:
                raise HTTPBoundError("response-too-large")
        remaining()
        return response.status, bytes(chunks)
    finally:
        connection.close()
