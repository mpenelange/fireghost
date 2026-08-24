"""Value-independent shape comparison.

We compare the *structure* (keys + value types) of crw's response against the
golden Firecrawl response, not the values — content legitimately differs
(timestamps, ids, the actual scraped markdown). A field "matches" when it is
present with a compatible type. This is exactly the compatibility question
issue #62 asks: does crw emit the same shape the SDK parses?
"""

from __future__ import annotations

from typing import Any


def type_name(v: Any) -> str:
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, int):
        return "int"
    if isinstance(v, float):
        return "float"
    if isinstance(v, str):
        return "str"
    if isinstance(v, list):
        return "array"
    if isinstance(v, dict):
        return "object"
    return type(v).__name__


def shape(v: Any) -> Any:
    """Structural signature: object -> {key: shape}, array -> [shape(first)],
    scalar -> type name."""
    if isinstance(v, dict):
        return {k: shape(val) for k, val in sorted(v.items())}
    if isinstance(v, list):
        return [shape(v[0])] if v else []
    return type_name(v)


def flatten(sig: Any, prefix: str = "") -> dict[str, str]:
    out: dict[str, str] = {}
    if isinstance(sig, dict):
        for k, val in sig.items():
            out.update(flatten(val, f"{prefix}.{k}" if prefix else k))
    elif isinstance(sig, list):
        if sig:
            out.update(flatten(sig[0], f"{prefix}[]"))
        else:
            out[prefix or "<root>"] = "array"
    else:
        out[prefix or "<root>"] = sig
    return out


def compare(golden: Any, actual: Any) -> dict[str, Any]:
    """Field-by-field shape comparison golden→actual.

    Returns present/total counts, the list of missing-or-mismatched fields, and
    a 0-100 score. int/float are interchangeable.
    """
    g = flatten(shape(golden))
    a = flatten(shape(actual))
    missing: list[str] = []
    ok = 0
    for field, gt in g.items():
        at = a.get(field)
        good = at is not None and (at == gt or {at, gt} <= {"int", "float"})
        if good:
            ok += 1
        else:
            missing.append(f"{field}({gt}→{at})")
    total = len(g) or 1
    return {
        "present": ok,
        "total": total,
        "missing": missing,
        "score": round(100 * ok / total, 1),
    }
