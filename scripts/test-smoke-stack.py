import importlib.util
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


spec = importlib.util.spec_from_file_location("smoke_stack", Path(__file__).with_name("smoke-stack.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class SmokeStackTests(unittest.TestCase):
    def test_api_redirect_does_not_forward_credentials_to_another_host(self):
        received = []

        class Destination(BaseHTTPRequestHandler):
            def record(self):
                received.append(dict(self.headers))
                self.send_response(200)
                self.end_headers()

            do_GET = record
            do_POST = record

            def log_message(self, *_args):
                pass

        destination = HTTPServer(("127.0.0.1", 0), Destination)
        thread = threading.Thread(target=destination.serve_forever, daemon=True)
        thread.start()

        class Redirect(BaseHTTPRequestHandler):
            def redirect(self):
                self.send_response(302)
                self.send_header("Location", f"http://127.0.0.1:{destination.server_port}/steal")
                self.end_headers()

            do_GET = redirect
            do_POST = redirect

            def log_message(self, *_args):
                pass

        origin = HTTPServer(("127.0.0.1", 0), Redirect)
        origin_thread = threading.Thread(target=origin.serve_forever, daemon=True)
        origin_thread.start()
        try:
            client = smoke.Client(f"http://127.0.0.1:{origin.server_port}", api_key="synthetic-key",
                                  bootstrap_token="synthetic-bootstrap")
            with self.assertRaises(smoke.SmokeError):
                client.request("POST", "/v1/tenants/register", {})
            self.assertEqual(received, [])
        finally:
            origin.shutdown()
            origin.server_close()
            destination.shutdown()
            destination.server_close()
            thread.join(timeout=1)
            origin_thread.join(timeout=1)

    def test_missing_route_fails_unless_explicitly_skipped(self):
        with self.assertRaisesRegex(smoke.SmokeError, "omitted its URL"):
            smoke.verify_route(None, None, False, [], [])
        passed, skipped = [], []
        smoke.verify_route(None, None, True, passed, skipped)
        self.assertEqual(passed, [])
        self.assertEqual(skipped, ["service HTTP route (explicitly skipped)"])

    def test_route_retries_transient_tls_failure_then_passes(self):
        class DelayedRoute:
            def __init__(self):
                self.attempts = 0

            def request(self, *_args, **_kwargs):
                self.attempts += 1
                if self.attempts == 1:
                    raise smoke.SmokeError("GET / failed (SSLCertVerificationError)")
                return 200, {"body": "ok"}

        client = DelayedRoute()
        passed = []
        smoke.verify_route(client, "https://smoke.example.invalid/", False, passed, [],
                           timeout=1, interval=0.001)
        self.assertEqual(client.attempts, 2)
        self.assertEqual(passed, ["service HTTP route"])


if __name__ == "__main__":
    unittest.main()
