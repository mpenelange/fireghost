"""Real loopback transport regressions; no external endpoints or containers."""

import contextlib
import socket
import threading
import time
import unittest
from unittest import mock

from scripts.gate_http import request_bytes


@contextlib.contextmanager
def raw_http_server(responder):
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(1)
    listener.settimeout(1)
    stop = threading.Event()
    peer_closed = threading.Event()
    finished = threading.Event()
    peers = []

    def serve():
        try:
            peer, _ = listener.accept()
            peers.append(peer)
            peer.settimeout(1)
            request = bytearray()
            while b"\r\n\r\n" not in request:
                part = peer.recv(4096)
                if not part:
                    peer_closed.set()
                    return
                request.extend(part)
            responder(peer, stop)
            if not peer.recv(1):
                peer_closed.set()
        except (BrokenPipeError, ConnectionResetError):
            peer_closed.set()
        except (TimeoutError, OSError):
            pass
        finally:
            for peer in peers:
                peer.close()
            finished.set()

    worker = threading.Thread(target=serve, daemon=True)
    worker.start()
    endpoint = "http://127.0.0.1:" + str(listener.getsockname()[1])
    try:
        yield endpoint, peer_closed, finished
    finally:
        stop.set()
        for peer in peers:
            with contextlib.suppress(OSError):
                peer.shutdown(socket.SHUT_RDWR)
            peer.close()
        listener.close()
        worker.join(timeout=0.2)


def trickle(peer, stop, prefix, repeated, suffix):
    peer.sendall(prefix)
    # Each byte arrives inside the idle timeout, but the entire framing takes
    # half a second. A total deadline must interrupt parsing rather than wait
    # for the header/chunk line to finish.
    for _ in range(20):
        if stop.wait(0.025):
            return
        peer.sendall(repeated)
    peer.sendall(suffix)


class GateHTTPDeadlineTests(unittest.TestCase):
    def assert_trickle_deadline(self, prefix, repeated, suffix):
        responder = lambda peer, stop: trickle(peer, stop, prefix, repeated, suffix)
        with raw_http_server(responder) as (endpoint, peer_closed, finished):
            started = time.monotonic()
            with self.assertRaises(TimeoutError):
                request_bytes(endpoint, "", "GET", "/health", None, 0.16, 1024)
            elapsed = time.monotonic() - started
            self.assertLess(elapsed, 0.32, "caller must return within the total deadline plus scheduling margin")
            self.assertTrue(peer_closed.wait(0.15), "deadline must interrupt the established connection")
            self.assertTrue(finished.wait(0.05), "mock server must finish after the client closes")

    def test_trickled_header_line_cannot_renew_total_deadline(self):
        self.assert_trickle_deadline(
            b"HTTP/1.1 200 OK\r\nX-Long: ", b"x",
            b"\r\nContent-Length: 0\r\n\r\n",
        )

    def test_trickled_chunk_size_cannot_renew_total_deadline(self):
        self.assert_trickle_deadline(
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n",
            b"0", b"1\r\nb\r\n0\r\n\r\n",
        )

    def test_slow_dns_is_bounded_without_sending_a_late_http_request(self):
        resolve = socket.getaddrinfo
        entered = threading.Event()
        release = threading.Event()
        late_request = threading.Event()

        def delayed(host, *args, **kwargs):
            if host == "deadline.test":
                entered.set()
                release.wait(0.5)
                host = "127.0.0.1"
            return resolve(host, *args, **kwargs)

        def reply(peer, stop):
            late_request.set()
            peer.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")

        with raw_http_server(reply) as (endpoint, peer_closed, finished):
            endpoint = endpoint.replace("127.0.0.1", "deadline.test")
            with mock.patch("socket.getaddrinfo", side_effect=delayed):
                started = time.monotonic()
                try:
                    with self.assertRaises(TimeoutError):
                        request_bytes(endpoint, "", "GET", "/health", None, 0.12, 1024)
                    self.assertTrue(entered.is_set())
                    self.assertLess(time.monotonic() - started, 0.24, "DNS must share the caller deadline")
                finally:
                    release.set()
                self.assertTrue(finished.wait(0.15), "released DNS worker must close without sending a request")
                self.assertTrue(peer_closed.is_set())
                self.assertFalse(late_request.is_set(), "a DNS result after the deadline must not send an HTTP request")

    def test_completed_response_closes_even_when_connection_detaches_socket(self):
        def reply(peer, stop):
            peer.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")

        with raw_http_server(reply) as (endpoint, peer_closed, finished):
            self.assertEqual(request_bytes(endpoint, "", "GET", "/health", None, 0.5, 1024), (200, b"{}"))
            self.assertTrue(peer_closed.wait(0.15))
            self.assertTrue(finished.wait(0.05))


if __name__ == "__main__":
    unittest.main()
