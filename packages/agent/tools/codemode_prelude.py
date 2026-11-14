import os
import sys
import json
import types
import traceback
import threading


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


class ToolError(Exception):
    def __init__(self, result):
        self.result = result
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
        return lambda **args: call(name, **args)


def search(query, limit=10):
    return _rpc("search", {"query": query, "limit": limit})["results"]


def describe(name):
    return _rpc("describe", {"name": name})


def result(value):
    _rpc("result", {"value": value})


zut = types.ModuleType("zut")
for _name in ("call", "search", "describe", "result", "ToolResult", "ToolDenied",
              "ToolError", "CodemodeError"):
    setattr(zut, _name, globals()[_name])
zut.tools = _Tools()
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
