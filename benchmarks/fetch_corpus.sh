#!/usr/bin/env bash
# Populate ./corpus with representative benchmark inputs. Large files are not
# committed; this script regenerates them.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
corpus="$here/corpus"
mkdir -p "$corpus"

# text: Go stdlib net/http source concatenated.
find "$(go env GOROOT)/src/net/http" -name '*.go' 2>/dev/null | head -40 \
  | xargs cat > "$corpus/text.txt" 2>/dev/null || true

# binary: a 3 MB prefix of the go tool binary.
head -c 3000000 "$(go env GOROOT)/bin/go" > "$corpus/binary.bin" 2>/dev/null || true

# json: a synthetic record array.
python3 - "$corpus" <<'PY'
import json, random, sys
random.seed(1)
rows=[]
for i in range(8000):
    rows.append({"id":i,"name":"user_%d"%i,"email":"user%d@example.com"%i,
      "active":bool(random.getrandbits(1)),"score":round(random.random()*100,3),
      "tags":[random.choice(["a","bb","ccc","dddd"]) for _ in range(random.randint(0,4))],
      "note":"lorem ipsum dolor sit amet "*random.randint(0,3)})
open(sys.argv[1]+"/data.json","w").write(json.dumps(rows))
PY
echo "corpus ready in $corpus"
