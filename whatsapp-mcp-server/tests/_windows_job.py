"""Minimal private job for descendants whose intermediate parents have exited.

Job inheritance and KILL_ON_JOB_CLOSE are documented at:
https://learn.microsoft.com/windows/win32/procthread/job-objects
This module is imported only on Windows; no pywin32 dependency is needed.
"""

import ctypes
from ctypes import wintypes as wt


class _BasicLimits(ctypes.Structure):
    _fields_ = [
        ("process_time", ctypes.c_longlong),
        ("job_time", ctypes.c_longlong),
        ("flags", wt.DWORD),
        ("min_working_set", ctypes.c_size_t),
        ("max_working_set", ctypes.c_size_t),
        ("active_processes", wt.DWORD),
        ("affinity", ctypes.c_size_t),
        ("priority", wt.DWORD),
        ("scheduling", wt.DWORD),
    ]


class _ExtendedLimits(ctypes.Structure):
    _fields_ = [
        ("basic", _BasicLimits),
        ("io_counters", ctypes.c_ulonglong * 6),
        ("process_memory", ctypes.c_size_t),
        ("job_memory", ctypes.c_size_t),
        ("peak_process_memory", ctypes.c_size_t),
        ("peak_job_memory", ctypes.c_size_t),
    ]


_api = ctypes.WinDLL("kernel32", use_last_error=True)
for _name, _result, _args in (
    ("CreateJobObjectW", wt.HANDLE, [wt.LPVOID, wt.LPCWSTR]),
    ("SetInformationJobObject", wt.BOOL, [wt.HANDLE, ctypes.c_int, wt.LPVOID, wt.DWORD]),
    ("AssignProcessToJobObject", wt.BOOL, [wt.HANDLE, wt.HANDLE]),
    ("CloseHandle", wt.BOOL, [wt.HANDLE]),
):
    _function = getattr(_api, _name)
    _function.restype, _function.argtypes = _result, _args


class WindowsJob:
    def __init__(self):
        self.handle = _api.CreateJobObjectW(None, None)
        if not self.handle:
            raise ctypes.WinError(ctypes.get_last_error())
        limits = _ExtendedLimits()
        limits.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if not _api.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            error = ctypes.WinError(ctypes.get_last_error())
            _api.CloseHandle(self.handle)
            raise error

    def assign(self, process_handle):
        # Popen owns this handle: no PID lookup or system-wide thread enumeration.
        if not _api.AssignProcessToJobObject(self.handle, int(process_handle)):
            raise ctypes.WinError(ctypes.get_last_error())

    def close(self):
        if self.handle:
            handle, self.handle = self.handle, None
            if not _api.CloseHandle(handle):
                raise ctypes.WinError(ctypes.get_last_error())
