"""Bound retained names under many identities and remove unvisited expiry."""

import whatsapp


def test_ten_thousand_names_are_bounded_and_expired_put_sweeps(monkeypatch):
    whatsapp._reset_name_cache()
    clock = [1000.0]
    monkeypatch.setattr(whatsapp.time, "monotonic", lambda: clock[0])
    try:
        for index in range(10000):
            whatsapp._cache_put("sender", f"fake-{index}", "Synthetic name")
        assert len(whatsapp._name_cache) == whatsapp.NAME_CACHE_MAX_ENTRIES == 4096
        assert whatsapp._cache_get("sender", "fake-0") == (False, None)
        assert whatsapp._cache_get("sender", "fake-9999") == (True, "Synthetic name")
        # Keys never queried again must also be removed when a new name arrives.
        clock[0] += whatsapp.NAME_CACHE_TTL_S + 1
        whatsapp._cache_put("sender", "fake-final", "Final synthetic name")
        assert set(whatsapp._name_cache) == {("sender", "fake-final")}
    finally:
        whatsapp._reset_name_cache()


def test_refreshed_key_survives_oldest_entry_eviction(monkeypatch):
    whatsapp._reset_name_cache()
    monkeypatch.setattr(whatsapp, "NAME_CACHE_MAX_ENTRIES", 2)
    try:
        whatsapp._cache_put("sender", "fake-0", "first")
        whatsapp._cache_put("sender", "fake-1", "second")
        whatsapp._cache_put("sender", "fake-0", "refreshed")
        whatsapp._cache_put("sender", "fake-2", "third")
        assert whatsapp._cache_get("sender", "fake-0") == (True, "refreshed")
        assert whatsapp._cache_get("sender", "fake-1") == (False, None)
        assert len(whatsapp._name_cache) == 2
    finally:
        whatsapp._reset_name_cache()
