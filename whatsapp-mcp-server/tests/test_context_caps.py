"""list_messages caps context windows and the total row budget."""

import main


def test_each_side_capped():
    assert main._cap_context(10, True, 500, 500) == (50, 50)


def test_context_disabled_means_zero():
    assert main._cap_context(10, False, 5, 5) == (0, 0)


def test_windows_shrink_to_row_budget():
    before, after = main._cap_context(500, True, 50, 50)
    assert before + after <= main.MAX_RESULT_ROWS // 500 - 1
    assert 500 * (1 + before + after) <= main.MAX_RESULT_ROWS


def test_small_requests_untouched():
    assert main._cap_context(50, True, 1, 1) == (1, 1)
    assert main._cap_context(0, True, 3, 4) == (3, 4)


def test_tool_passes_capped_values(monkeypatch):
    seen = {}

    def fake(**kwargs):
        seen.update(kwargs)
        return []

    monkeypatch.setattr(main, "whatsapp_list_messages", fake)
    main.list_messages(limit=5000, context_before=999, context_after=999)
    assert seen["limit"] == 500
    assert seen["context_before"] + seen["context_after"] <= main.MAX_RESULT_ROWS // 500 - 1
