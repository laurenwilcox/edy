# Скелет Python-файлов для Context Engine Edy.
# Читает пути файлов (по одному в строке) из stdin, печатает JSON:
# {"путь": {"items": [{"kind", "name", "sig", "line", "end"}], "error": ""}}
import ast
import json
import sys


def args_of(fn):
    a = fn.args
    names = [x.arg for x in a.posonlyargs + a.args]
    if a.vararg:
        names.append("*" + a.vararg.arg)
    names += [x.arg for x in a.kwonlyargs]
    if a.kwarg:
        names.append("**" + a.kwarg.arg)
    return ", ".join(names)


def visit(node, prefix, items):
    for n in getattr(node, "body", []):
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)):
            kw = "async def" if isinstance(n, ast.AsyncFunctionDef) else "def"
            items.append({"kind": "def", "name": prefix + n.name, "sig": f"{kw} {n.name}({args_of(n)})",
                          "line": n.lineno, "end": getattr(n, "end_lineno", n.lineno)})
        elif isinstance(n, ast.ClassDef):
            bases = ", ".join(ast.unparse(b) for b in n.bases) if hasattr(ast, "unparse") else ""
            sig = f"class {n.name}({bases})" if bases else f"class {n.name}"
            items.append({"kind": "class", "name": prefix + n.name, "sig": sig,
                          "line": n.lineno, "end": getattr(n, "end_lineno", n.lineno)})
            visit(n, prefix + n.name + ".", items)


out = {}
for path in sys.stdin.read().splitlines():
    if not path:
        continue
    try:
        with open(path, encoding="utf-8") as f:
            tree = ast.parse(f.read(), filename=path)
    except Exception as e:  # синтаксическая ошибка, кодировка и т.п.
        out[path] = {"items": [], "error": str(e)}
        continue
    items = []
    visit(tree, "", items)
    out[path] = {"items": items, "error": ""}
json.dump(out, sys.stdout)
