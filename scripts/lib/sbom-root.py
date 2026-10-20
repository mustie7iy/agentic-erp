"""Puts an image's own package first in syft's SPDX SBOM, named by the
image's digest: sign-images.sh finds the SBOM of an image by
.packages[0].name, as ko writes them. Used by mirror-images.sh and
images.sh (erp-analytics).

    python3 sbom-root.py RAW OUT DIGEST SOURCE_INFO
"""
import json
import sys

raw, out, digest, origin = sys.argv[1:]
doc = json.load(open(raw))
roots = [p for p in doc["packages"] if p.get("primaryPackagePurpose") == "CONTAINER"]
if len(roots) != 1:
    sys.exit("sbom-root: %s: %d container packages in the SBOM, want 1" % (origin, len(roots)))
root = roots[0]
root["name"], root["sourceInfo"] = digest, origin
doc["packages"] = [root] + [p for p in doc["packages"] if p is not root]
json.dump(doc, open(out, "w"))
