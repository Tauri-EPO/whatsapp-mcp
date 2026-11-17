"""Private test process tree, contained before the first instruction executes.

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


class _ThreadEntry(ctypes.Structure):
    _fields_ = [
        ("size", wt.DWORD),
        ("usage", wt.DWORD),
        ("thread", wt.DWORD),
        ("owner", wt.DWORD),
        ("priority", wt.LONG),
        ("delta", wt.LONG),
        ("flags", wt.DWORD),
    ]


_api = ctypes.WinDLL("kernel32", use_last_error=True)
for _name, _result, _args in (
    ("CreateJobObjectW", wt.HANDLE, [wt.LPVOID, wt.LPCWSTR]),
    ("SetInformationJobObject", wt.BOOL, [wt.HANDLE, ctypes.c_int, wt.LPVOID, wt.DWORD]),
    ("OpenProcess", wt.HANDLE, [wt.DWORD, wt.BOOL, wt.DWORD]),
    ("AssignProcessToJobObject", wt.BOOL, [wt.HANDLE, wt.HANDLE]),
    ("CloseHandle", wt.BOOL, [wt.HANDLE]),
    ("CreateToolhelp32Snapshot", wt.HANDLE, [wt.DWORD, wt.DWORD]),
    ("Thread32First", wt.BOOL, [wt.HANDLE, ctypes.POINTER(_ThreadEntry)]),
    ("Thread32Next", wt.BOOL, [wt.HANDLE, ctypes.POINTER(_ThreadEntry)]),
    ("OpenThread", wt.HANDLE, [wt.DWORD, wt.BOOL, wt.DWORD]),
    ("ResumeThread", wt.DWORD, [wt.HANDLE]),
):
    _function = getattr(_api, _name)
    _function.restype, _function.argtypes = _result, _args


class WindowsJob:
    def __init__(self):
        self.handle = _api.CreateJobObjectW(None, None)
        if not self.handle:
            raise ctypes.WinError(ctypes.get_last_error())
        limits = _ExtendedLimits()
        limits.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; no breakaway.
        if not _api.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            error = ctypes.WinError(ctypes.get_last_error())
            self.close()
            raise error

    def assign_and_resume(self, pid):
        # Popen created this process with CREATE_SUSPENDED: it cannot spawn an
        # uncontained child between process creation and job assignment.
        process = _api.OpenProcess(0x0101, False, pid)  # SET_QUOTA | TERMINATE
        if not process:
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            if not _api.AssignProcessToJobObject(self.handle, process):
                raise ctypes.WinError(ctypes.get_last_error())
        finally:
            _api.CloseHandle(process)
        snapshot = _api.CreateToolhelp32Snapshot(4, 0)  # TH32CS_SNAPTHREAD
        if snapshot == ctypes.c_void_p(-1).value:
            raise ctypes.WinError(ctypes.get_last_error())
        resumed = False
        try:
            entry = _ThreadEntry(size=ctypes.sizeof(_ThreadEntry))
            more = _api.Thread32First(snapshot, ctypes.byref(entry))
            while more:
                if entry.owner == pid:
                    thread = _api.OpenThread(2, False, entry.thread)  # THREAD_SUSPEND_RESUME
                    if not thread:
                        raise ctypes.WinError(ctypes.get_last_error())
                    try:
                        count = _api.ResumeThread(thread)
                        if count == 0xFFFFFFFF:
                            raise ctypes.WinError(ctypes.get_last_error())
                        resumed |= count > 0
                    finally:
                        _api.CloseHandle(thread)
                more = _api.Thread32Next(snapshot, ctypes.byref(entry))
        finally:
            _api.CloseHandle(snapshot)
        if not resumed:
            raise RuntimeError("test process had no suspended thread to resume")

    def close(self):
        if self.handle:
            # The handle is private and non-inheritable: closing it kills all
            # members, including descendants whose original parent exited.
            _api.CloseHandle(self.handle)
            self.handle = None
