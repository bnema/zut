import os
import sys
import json
import types
import traceback
import threading
import difflib


class CodemodeError(Exception):
    def __init__(self, kind, message):
        self.kind = kind
        super().__init__(message)


class ToolDenied(CodemodeError):
    pass


class ToolResult:
    def __init__(self, value):
        self.id = value["id"]
        self.text = value["text"]
        self.data = value["data"]
        self.is_error = value["is_error"]

    def __repr__(self):
        preview = self.text[:80] + ("…" if len(self.text) > 80 else "")
        return f"ToolResult(id={self.id!r}, is_error={self.is_error!r}, text={preview!r})"

    def __str__(self):
        return self.text

    def __bool__(self):
        return not self.is_error


class ToolError(Exception):
    def __init__(self, result):
        self.result = result
        self.kind = "tool_error"
        super().__init__(result.text)


_handles = [int(v) for v in os.environ["ZUT_CODEMODE_FDS"].split(",")]
if os.name == "nt":
    import msvcrt
    _fds = [msvcrt.open_osfhandle(_handles[0], os.O_WRONLY),
            msvcrt.open_osfhandle(_handles[1], os.O_RDONLY)]
else:
    _fds = _handles
for _fd in _fds:
    os.set_inheritable(_fd, False)
_requests = os.fdopen(_fds[0], "w", buffering=1, encoding="utf-8")
_responses = os.fdopen(_fds[1], "r", encoding="utf-8")

_rpc_lock = threading.Lock()


def _rpc(method, params):
    request = json.dumps({"method": method, "params": params})
    if len(request.encode("utf-8")) > 8 * 1024 * 1024:
        raise CodemodeError("limit", "request exceeds 8 MiB")
    with _rpc_lock:
        try:
            _requests.write(request + "\n")
            _requests.flush()
            line = _responses.readline()
        except (OSError, ValueError) as error:
            raise CodemodeError("bad_request", "codemode bridge closed: " + str(error)) from error
    if not line:
        raise CodemodeError("closed", "codemode host disconnected")
    reply = json.loads(line)
    if "error" in reply:
        error = reply["error"]
        cls = ToolDenied if error["kind"] == "denied" else CodemodeError
        raise cls(error["kind"], error["message"])
    return reply["result"]


def call(name, /, check=True, **args):
    result = ToolResult(_rpc("call", {"name": name, "args": args}))
    if check and result.is_error:
        raise ToolError(result)
    return result


class _Tools:
    def __getattr__(self, name, /):
        if name.startswith("__"):
            raise AttributeError(name)
        return lambda check=True, **args: call(name, check=check, **args)

    def __getitem__(self, name, /):
        return self.__getattr__(name)

    def __dir__(self):
        return _rpc("list", {})


class _MCP:
    def __init__(self, server=None):
        self.__server = server

    def __dir__(self):
        namespaces = _rpc("namespaces", {})
        return sorted(namespaces if self.__server is None else namespaces.get(self.__server, {}))

    def __getattr__(self, name, /):
        if name.startswith("__"):
            raise AttributeError(name)
        namespaces = _rpc("namespaces", {})
        entries = namespaces if self.__server is None else namespaces.get(self.__server, {})
        if name not in entries:
            suggestions = difflib.get_close_matches(name, entries, n=3)
            message = f"unknown MCP {'server' if self.__server is None else 'method'} {name!r}"
            if suggestions:
                message += "; did you mean: " + ", ".join(suggestions) + "?"
            raise AttributeError(message)
        if self.__server is None:
            return _MCP(name)
        canonical = entries[name]
        def invoke(check=True, **args):
            return call(canonical, check=check, **args)
        invoke.__name__ = canonical
        invoke._zut_tool_name = canonical
        return invoke


class SearchResults(list):
    def __init__(self, value):
        super().__init__(value["results"])
        self.total = value["total"]
        self.truncated = value["truncated"]


def search(query, limit=10):
    return SearchResults(_rpc("search", {"query": query, "limit": limit}))


def describe(name):
    return _rpc("describe", {"name": name})


def help(name):
    if callable(name):
        canonical = getattr(name, "_zut_tool_name", None)
        if not isinstance(canonical, str):
            raise TypeError("help expects a tool name or zut.mcp callable")
        details = describe(canonical)
        name.__doc__ = details["signature"] + "\n" + details["description"]
    else:
        details = describe(name)
    print(details["signature"])
    print(details["description"])


def result(value):
    _rpc("result", {"value": value})


zut = types.ModuleType("zut")
for _name in ("call", "search", "describe", "help", "result", "ToolResult", "ToolDenied",
              "ToolError", "CodemodeError", "SearchResults"):
    setattr(zut, _name, globals()[_name])
zut.tools = _Tools()
zut.mcp = _MCP()
sys.modules["zut"] = zut
_code = _rpc("init", {})["code"]
os.environ.pop("ZUT_CODEMODE_FDS", None)
_prelude_filename = sys._getframe().f_code.co_filename
try:
    exec(compile(_code, "<codemode>", "exec"), {"__name__": "__main__", "zut": zut})
except (SystemExit, KeyboardInterrupt):
    raise
except BaseException as _error:
    # TracebackException preserves exception chaining and imported-code frames.
    # Filter only this prelude's frames, including those in causes/contexts.
    _formatted = traceback.TracebackException.from_exception(_error)
    _pending = [_formatted]
    while _pending:
        _exception = _pending.pop()
        _exception.stack = traceback.StackSummary.from_list([
            frame for frame in _exception.stack
            if frame.filename != _prelude_filename
        ])
        if _exception.__cause__ is not None:
            _pending.append(_exception.__cause__)
        if _exception.__context__ is not None:
            _pending.append(_exception.__context__)
    sys.stderr.write("".join(_formatted.format(chain=True)))
    sys.exit(1)
