"""Count the tokens of synthetic prompts with tiktoken.

Reads the JSON lines printed by `go run ./scripts/prompttokens` from stdin
and encodes every prompt with the cl100k_base and o200k_base encodings.
Prints one summary line per encoding and exits with status 1 if any prompt
differs from its target length.
"""

import json
import sys
from collections import Counter

import tiktoken

ENCODINGS = ("cl100k_base", "o200k_base")


def main() -> int:
    encodings = {name: tiktoken.get_encoding(name) for name in ENCODINGS}
    prompts = [json.loads(line) for line in sys.stdin if line.strip()]
    if not prompts:
        print("no prompts on stdin", file=sys.stderr)
        return 1
    failed = False
    for name, enc in encodings.items():
        exact = 0
        off = Counter()
        for p in prompts:
            n = len(enc.encode(p["text"]))
            if n == p["target"]:
                exact += 1
            else:
                off[(p["target"], n)] += 1
        targets = sorted({p["target"] for p in prompts})
        print(f"{name}: {exact} of {len(prompts)} prompts exact (targets {targets[0]} to {targets[-1]})")
        for (target, got), times in sorted(off.items()):
            print(f"  target {target}: {got} tokens in {times} prompts", file=sys.stderr)
        failed = failed or bool(off)
    print(f"tiktoken {tiktoken.__version__}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
