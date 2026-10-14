#!/usr/bin/env python3
"""Exercise a fresh agentic-hosting stack through its public HTTP API."""

import argparse
import json
import os
import secrets
import socket
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


IMAGE = "traefik/whoami:v1.11.0"


class SmokeError(Exception):
    pass


class Client:
    def __init__(self, base_url, api_key=None, forwarded_proto=False, bootstrap_token=None):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.forwarded_proto = forwarded_proto
        self.bootstrap_token = bootstrap_token

    def request(self, method, path, body=None, timeout=45, raw_url=None):
        url = raw_url or (self.base_url + path)
        headers = {"Accept": "application/json"}
        if self.api_key and not raw_url:
            headers["Authorization"] = "Bearer " + self.api_key
        if self.bootstrap_token and path == "/v1/tenants/register":
            headers["X-Bootstrap-Token"] = self.bootstrap_token
        if self.forwarded_proto and not raw_url:
            headers["X-Forwarded-Proto"] = "https"
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout, context=ssl.create_default_context()) as response:
                payload = response.read()
                if not payload:
                    return response.status, None
                try:
                    return response.status, json.loads(payload)
                except (UnicodeDecodeError, json.JSONDecodeError):
                    return response.status, payload
        except urllib.error.HTTPError as exc:
            # Do not read or print bodies: API errors may include secrets.
            raise SmokeError(f"{method} {path or '/'} returned HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, socket.timeout, OSError) as exc:
            reason = getattr(exc, "reason", None)
            kind = type(reason or exc).__name__
            raise SmokeError(f"{method} {path or '/'} failed ({kind})") from None


def require(condition, message):
    if not condition:
        raise SmokeError(message)


def wait_for(label, fetch, accepted, timeout, passed, interval=5):
    end = time.monotonic() + timeout
    last = "unknown"
    while time.monotonic() < end:
        obj = fetch()
        last = obj.get("status", "unknown") if isinstance(obj, dict) else "invalid response"
        if isinstance(obj, dict) and accepted(obj):
            passed.append(label)
            print(f"PASS {label}: {last}")
            return obj
        if last in ("failed", "circuit_open"):
            raise SmokeError(f"{label} entered {last}")
        time.sleep(min(interval, max(0, end - time.monotonic())))
    raise SmokeError(f"{label} timed out (last status: {last})")


def token_from_file(path):
    stat = os.stat(path)
    if stat.st_mode & 0o077:
        raise SmokeError("bootstrap token file must be readable only by its owner (mode 0600 or stricter)")
    with open(path, encoding="utf-8") as stream:
        token = stream.read().strip()
    if not token:
        raise SmokeError("bootstrap token file is empty")
    return token


def checked_url(base_url, insecure_local_http):
    parsed = urllib.parse.urlparse(base_url)
    require(parsed.scheme in ("http", "https") and parsed.hostname, "base URL must be an http(s) URL")
    try:
        loopback = ipaddress_is_loopback(parsed.hostname)
    except ValueError:
        loopback = parsed.hostname.lower() == "localhost"
    forwarded = parsed.scheme == "http" and loopback and insecure_local_http
    require(parsed.scheme == "https" or forwarded,
            "base URL must use public HTTPS, or loopback HTTP with --loopback-proxy")
    require(not (parsed.scheme == "http" and not loopback), "plain HTTP is allowed only to loopback")
    return forwarded


def ipaddress_is_loopback(host):
    import ipaddress
    return ipaddress.ip_address(host).is_loopback


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.environ.get("AH_BASE_URL"))
    parser.add_argument("--bootstrap-token-file", default=os.environ.get("AH_BOOTSTRAP_TOKEN_FILE"))
    parser.add_argument("--loopback-proxy", action="store_true",
                        help="send X-Forwarded-Proto:https to a loopback HTTP API")
    parser.add_argument("--skip-public-route", action="store_true",
                        help="report route verification as SKIPPED when DNS/ingress is not ready")
    parser.add_argument("--service-timeout", type=int, default=600)
    parser.add_argument("--environment-timeout", type=int, default=240)
    args = parser.parse_args()

    if not args.base_url or not args.bootstrap_token_file:
        parser.error("--base-url and --bootstrap-token-file (or AH_BASE_URL/AH_BOOTSTRAP_TOKEN_FILE) are required")
    if args.service_timeout < 1 or args.environment_timeout < 1:
        parser.error("timeouts must be positive")

    created = {"service": None, "environment": None, "databases": [],
               "service_name": None, "environment_name": None, "database_names": []}
    tenant_client = None
    registration_succeeded = False
    failures = []
    passed = []
    skipped = []

    def record_pass(label):
        passed.append(label)
        print(f"PASS {label}")

    try:
        bootstrap = token_from_file(args.bootstrap_token_file)
        forwarded = checked_url(args.base_url, args.loopback_proxy)
        public = Client(args.base_url, forwarded_proto=forwarded, bootstrap_token=bootstrap)
        suffix = secrets.token_hex(5)
        tenant_name = "smoke-" + suffix
        status, registration = public.request("POST", "/v1/tenants/register",
                                              {"name": tenant_name, "email": f"{tenant_name}@example.invalid"})
        registration_succeeded = status == 201
        require(status == 201 and isinstance(registration, dict), "tenant registration returned an unexpected response")
        api_key = registration.get("api_key")
        tenant_id = registration.get("tenant_id")
        require(api_key and tenant_id, "tenant registration response omitted required fields")
        tenant_client = Client(args.base_url, api_key, forwarded)
        record_pass("created isolated synthetic tenant")

        created["service_name"] = "smoke-web-" + suffix
        status, service = tenant_client.request("POST", "/v1/services",
            {"name": created["service_name"], "image": IMAGE, "port": 80})
        require(status == 201 and isinstance(service, dict) and service.get("id"),
                "service creation returned an unexpected response")
        created["service"] = service["id"]
        service_path = "/v1/services/" + urllib.parse.quote(created["service"], safe="")
        service = wait_for("HTTP service deployment", lambda: tenant_client.request("GET", service_path)[1],
                           lambda obj: obj.get("status") == "running", args.service_timeout, passed)
        route = service.get("url")
        if not route:
            skipped.append("service HTTP route (API returned no URL)")
            print("SKIPPED service HTTP route: API returned no URL")
        elif args.skip_public_route:
            skipped.append("service HTTP route (explicitly skipped)")
            print("SKIPPED service HTTP route: --skip-public-route")
        else:
            route_parts = urllib.parse.urlparse(route)
            require(route_parts.scheme in ("http", "https") and route_parts.hostname and
                    not route_parts.username and not route_parts.password,
                    "service route URL was not a valid HTTP(S) URL")
            try:
                route_status, _ = tenant_client.request("GET", "", timeout=15, raw_url=route)
                require(200 <= route_status < 300, "service route did not return a 2xx response")
                record_pass("service HTTP route returned 2xx")
            except SmokeError:
                if not args.skip_public_route:
                    raise

        created["environment_name"] = "smoke-env-" + suffix
        status, env = tenant_client.request("POST", "/v1/environments",
            {"name": created["environment_name"], "template_id": "default"}, timeout=90)
        require(status == 201 and isinstance(env, dict) and env.get("id"),
                "environment creation returned an unexpected response")
        created["environment"] = env["id"]
        env_path = "/v1/environments/" + urllib.parse.quote(created["environment"], safe="")
        wait_for("environment", lambda: tenant_client.request("GET", env_path)[1],
                 lambda obj: obj.get("status") == "running", args.environment_timeout, passed)
        exec_path = env_path + "/exec"
        status, result = tenant_client.request("POST", exec_path,
            {"command": ["sh", "-lc", "printf smoke-ok; exit 0"], "timeout": 30}, timeout=45)
        require(status == 200 and isinstance(result, dict) and result.get("exit_code") == 0 and
                result.get("stdout", "").strip() == "smoke-ok", "environment successful exec check failed")
        record_pass("environment exec returned expected output and exit code 0")
        status, result = tenant_client.request("POST", exec_path,
            {"command": ["sh", "-lc", "exit 7"], "timeout": 30}, timeout=45)
        require(status == 200 and isinstance(result, dict) and result.get("exit_code") == 7,
                "environment nonzero exec check failed")
        record_pass("environment exec preserved nonzero exit code")

        for kind in ("redis", "postgres"):
            name = "smoke-" + kind + "-" + suffix
            created["database_names"].append(name)
            status, database = tenant_client.request("POST", "/v1/databases",
                {"name": name, "type": kind}, timeout=60)
            require(status == 201 and isinstance(database, dict) and database.get("id"),
                    f"{kind} provisioning returned an unexpected response")
            created["databases"].append(database["id"])
            db_path = "/v1/databases/" + urllib.parse.quote(database["id"], safe="")
            wait_for(kind + " database protocol readiness",
                     lambda path=db_path: tenant_client.request("GET", path)[1],
                     lambda obj, expected=kind: obj.get("status") == "ready" and obj.get("type") == expected,
                     45, passed, interval=3)
    except SmokeError as exc:
        failures.append(str(exc))
    except (OSError, ValueError) as exc:
        failures.append(f"input or local setup failed ({type(exc).__name__})")

    if tenant_client:
        # Recover IDs if a successful server-side create timed out before returning its response.
        for key, endpoint, name_field, target in (
            ("service", "/v1/services", "service_name", "service"),
            ("environment", "/v1/environments", "environment_name", "environment"),
        ):
            if created[target] is None and created[name_field]:
                try:
                    _, listing = tenant_client.request("GET", endpoint)
                    if isinstance(listing, list):
                        found = next((item.get("id") for item in listing
                                      if isinstance(item, dict) and item.get("name") == created[name_field]), None)
                        if found:
                            created[target] = found
                except SmokeError:
                    pass
        if len(created["databases"]) < len(created["database_names"]):
            try:
                _, listing = tenant_client.request("GET", "/v1/databases")
                if isinstance(listing, list):
                    known = set(created["databases"])
                    for item in listing:
                        if (isinstance(item, dict) and item.get("name") in created["database_names"]
                                and item.get("id") not in known):
                            created["databases"].append(item["id"])
                            known.add(item["id"])
            except SmokeError:
                pass
        resources = []
        if created["environment"]:
            resources.append(("environment", "/v1/environments/" + urllib.parse.quote(created["environment"], safe="")))
        if created["service"]:
            resources.append(("service", "/v1/services/" + urllib.parse.quote(created["service"], safe="")))
        for db_id in created["databases"]:
            resources.append(("database", "/v1/databases/" + urllib.parse.quote(db_id, safe="")))
        for label, path in resources:
            try:
                tenant_client.request("DELETE", path)
                print(f"CLEANED {label}")
            except SmokeError as exc:
                failures.append("cleanup " + str(exc))
        try:
            tenant_client.request("DELETE", "/v1/tenant")
            print("CLEANED synthetic tenant (suspended and API key revoked)")
        except SmokeError as exc:
            failures.append("cleanup " + str(exc))

    for failure in failures:
        print("FAIL " + failure, file=sys.stderr)
    if registration_succeeded and tenant_client is None:
        print("FAIL synthetic tenant created but no authenticated cleanup client was established", file=sys.stderr)
        failures.append("synthetic tenant could not be authenticated for cleanup")
    if failures:
        print(f"RESULT failed: {len(passed)} passed, {len(skipped)} skipped, {len(failures)} failed")
        return 1
    print(f"RESULT passed: {len(passed)} passed, {len(skipped)} skipped, cleanup complete")
    return 0


if __name__ == "__main__":
    sys.exit(main())
