import re
import sys
from pathlib import Path

import yaml


WORKFLOWS = Path(".github/workflows")
PINS = (
    "LLVM_MINGW_VERSION", "LLVM_MINGW_DIR", "LLVM_MINGW_SHA256",
    "PYWIN32_VERSION", "PYWIN32_WHEEL_DIR",
)
CACHE_PATHS = {"${{ env.LLVM_MINGW_DIR }}", "${{ env.PYWIN32_WHEEL_DIR }}"}


def contract(filename, job_name, action):
    path = WORKFLOWS / filename
    job = yaml.load(path.read_text(encoding="utf-8"), Loader=yaml.BaseLoader)["jobs"][job_name]
    pins = {name: job["env"][name] for name in PINS}
    keys = {}
    for step in job["steps"]:
        if step.get("uses", "").partition("@")[0] != action:
            continue
        inputs = step["with"]
        cache_path = inputs["path"]
        if cache_path in keys or inputs.get("restore-keys"):
            sys.exit(f"{path}: expected one exact key per cache path, got {inputs}")
        keys[cache_path] = inputs["key"]
    if keys.keys() != CACHE_PATHS:
        sys.exit(f"{path}: unexpected {action} cache paths: {sorted(keys)}")
    return pins, keys


def main():
    reader, restore_keys = contract("builder-windows-arm64.yml", "luci", "actions/cache/restore")
    writer, save_keys = contract("cache-windows-arm64.yml", "populate", "actions/cache")
    for name in PINS:
        if reader[name] != writer[name]:
            sys.exit(f"{name} mismatch: reader {reader[name]!r}, writer {writer[name]!r}")
    for path in sorted(CACHE_PATHS):
        if restore_keys[path] != save_keys[path]:
            sys.exit(f"Cache key mismatch for {path}: reader {restore_keys[path]!r}, writer {save_keys[path]!r}")

    requirements = Path(".github/requirements-builder-windows-arm64.txt").read_text(encoding="utf-8")
    if re.findall(r"(?m)^pywin32==([0-9]+)\s*$", requirements) != [reader["PYWIN32_VERSION"]]:
        sys.exit("Requirements must pin the same pywin32 version as both workflows")
    print("Builder dependency pins, cache paths and exact cache keys match.")


if __name__ == "__main__":
    main()
