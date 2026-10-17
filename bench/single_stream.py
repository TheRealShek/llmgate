"""Compare sequential streaming requests to llama.cpp and the gateway."""

import argparse
import json
import math
import os
import statistics
import sys
import time
import urllib.request


PAYLOAD = {
    "model": "glm-edge-4b-chat",
    "messages": [{
        "role": "user",
        "content": "Explain why a gateway needs timeouts in three short sentences.",
    }],
    "stream": True,
    "max_tokens": 128,
    "temperature": 0,
}
TARGETS = [
    ("direct", "http://127.0.0.1:8081"),
    ("gateway", "http://127.0.0.1:8080"),
]


def measure(base, api_key=None):
    headers = {"Content-Type": "application/json"}
    if api_key is not None:
        headers["Authorization"] = "Bearer " + api_key

    request = urllib.request.Request(
        base + "/v1/chat/completions",
        data=json.dumps(PAYLOAD).encode(),
        headers=headers,
    )
    started = time.perf_counter()
    first_token = None
    timings = {}
    done = False
    finish_reason = None

    with urllib.request.urlopen(request, timeout=60) as response:
        for line in response:
            if not line.startswith(b"data:"):
                continue

            data = line[5:].strip()
            if data == b"[DONE]":
                done = True
                break

            event = json.loads(data)
            for choice in event.get("choices", []):
                if choice.get("delta", {}).get("content") and first_token is None:
                    first_token = time.perf_counter()
                if choice.get("finish_reason") is not None:
                    finish_reason = choice["finish_reason"]
            timings.update(event.get("timings", {}))

    finished = time.perf_counter()
    if first_token is None or not done:
        raise RuntimeError("Stream did not complete with text and [DONE]")

    return {
        "ttft_ms": (first_token - started) * 1000,
        "total_ms": (finished - started) * 1000,
        "backend_decode_tps": timings.get("predicted_per_second"),
        "completion_tokens": timings.get("predicted_n"),
        "finish_reason": finish_reason,
    }


def summarize(name, results):
    rows = [row for row in results if row["path"] == name]
    successes = [row for row in rows if "error" not in row]
    summary = {
        "path": name,
        "requests": len(rows),
        "successes": len(successes),
        "failures": len(rows) - len(successes),
        "completion_tokens": sorted({
            row["completion_tokens"] for row in successes
            if row["completion_tokens"] is not None
        }),
        "finish_reasons": sorted({
            row["finish_reason"] for row in successes
            if row["finish_reason"] is not None
        }),
    }
    for field in ["ttft_ms", "total_ms", "backend_decode_tps"]:
        values = sorted(row[field] for row in successes if row[field] is not None)
        summary[field] = {
            "median": statistics.median(values),
            "p95_nearest_rank": values[math.ceil(len(values) * 0.95) - 1],
            "min": min(values),
            "max": max(values),
        } if values else None
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--model",
        default=PAYLOAD["model"],
        help="Model ID advertised by /v1/models",
    )
    parser.add_argument(
        "--direct-only", action="store_true",
        help="Measure only llama.cpp, without a gateway or API key",
    )
    parser.add_argument(
        "--requests-per-path", type=int, default=50,
        help="Measured requests per path, excluding one warm-up (default: 50)",
    )
    args = parser.parse_args()
    if args.requests_per_path < 1:
        parser.error("--requests-per-path must be positive")
    PAYLOAD["model"] = args.model
    targets = TARGETS[:1] if args.direct_only else TARGETS
    api_key = os.environ.get("API_KEY")
    if not args.direct_only and not api_key:
        parser.error("Set API_KEY to an active gateway key before benchmarking")

    for name, base in targets:
        print(
            "Warm-up:", name,
            measure(base, api_key if name == "gateway" else None),
            file=sys.stderr, flush=True,
        )

    results = []
    total = args.requests_per_path * len(targets)
    for pair in range(1, args.requests_per_path + 1):
        order = targets if pair % 2 else list(reversed(targets))
        for name, base in order:
            row = {"pair": pair, "path": name}
            try:
                row.update(measure(base, api_key if name == "gateway" else None))
            except Exception as error:
                row["error"] = type(error).__name__ + ": " + str(error)
            results.append(row)

        if pair % 5 == 0 or pair == args.requests_per_path:
            failures = sum("error" in row for row in results)
            print(
                f"{len(results)}/{total} requests, {failures} failures",
                file=sys.stderr,
                flush=True,
            )

    report = {
        "payload": PAYLOAD,
        "concurrency": 1,
        "gateway_authentication": None if args.direct_only else "direct_postgres_lookup",
        "warmups_per_path": 1,
        "results": results,
        "summaries": [summarize(name, results) for name, _ in targets],
    }
    print(json.dumps(report, indent=2))
    return 1 if any("error" in row for row in results) else 0


if __name__ == "__main__":
    sys.exit(main())
