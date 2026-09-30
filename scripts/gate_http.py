"""Bounded, non-redirecting byte transport shared by validation gates."""

import http.client
import json
import socket
import threading
import time
import urllib.parse


MAX_REQUEST_BYTES = 65536
MAX_RESPONSE_BYTES = 16 * 1024 * 1024


class HTTPBoundError(Exception):
    """A fixed transport check identifier without upstream response content."""


def request_bytes(endpoint, api_key, method, path, payload, timeout, max_bytes):
    """Return status and bounded bytes; callers own status and JSON semantics.

    One caller deadline includes DNS, connection setup, headers, chunk framing,
    and the body. On expiry, shutdown interrupts an established socket and the
    daemon worker owns final cleanup. OS name resolution cannot be cancelled
    portably: a stuck resolver may retain one daemon worker until it returns,
    but cannot delay the caller or send a late HTTP request. Gates make a
    bounded number of requests, so this is not an unbounded request service.
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
    cancelled = threading.Event()
    completed = threading.Event()
    active_socket = [None]
    outcome = []

    def remaining():
        budget = deadline - time.monotonic()
        if cancelled.is_set() or budget <= 0:
            raise TimeoutError()
        return budget

    def exchange():
        response = None
        try:
            # Explicit setup lets cancellation be checked after a delayed DNS
            # result, before HTTPConnection.request can send credentials/body.
            remaining()
            connection.connect()
            active_socket[0] = connection.sock
            active_socket[0].settimeout(remaining())
            connection.request(method, parsed.path.rstrip("/") + path, body=body, headers=headers)
            active_socket[0].settimeout(remaining())
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
                budget = remaining()
                try:
                    active_socket[0].settimeout(budget)
                except OSError:
                    # A completed Connection: close response may have already
                    # closed the detached socket; read1 can still report EOF.
                    if active_socket[0].fileno() != -1:
                        raise
                chunk = response.read1(min(65536, max_bytes + 1 - len(chunks)))
                if not chunk:
                    break
                chunks.extend(chunk)
                if len(chunks) > max_bytes:
                    raise HTTPBoundError("response-too-large")
            remaining()
            outcome.append((response.status, bytes(chunks)))
        except BaseException as error:
            outcome.append(error)
        finally:
            # Response owns the buffered socket when getresponse detaches it
            # from the connection (Connection: close). Both resources close
            # even if one cleanup operation raises.
            for resource in (response, connection):
                if resource is None:
                    continue
                try:
                    close = getattr(resource, "close", None)
                    if close:
                        close()
                except BaseException as error:
                    if not outcome or not isinstance(outcome[0], BaseException):
                        outcome[:] = [error]
            completed.set()

    threading.Thread(target=exchange, name="gate-http", daemon=True).start()
    budget = deadline - time.monotonic()
    if budget <= 0 or not completed.wait(budget):
        cancelled.set()
        transport_socket = active_socket[0]
        if transport_socket is not None:
            try:
                transport_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        raise TimeoutError()
    remaining()
    result = outcome[0]
    if isinstance(result, BaseException):
        raise result
    return result
