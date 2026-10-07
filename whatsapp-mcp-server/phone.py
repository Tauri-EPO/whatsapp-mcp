"""The spellings of one phone number, for the contact lookups (issue #444).

A Brazilian mobile is written two ways: with the ninth digit that was put in
front of every mobile subscriber number (55 88 9 8195-2753) and without it
(55 88 8195-2753). WhatsApp registers the account under one of the two, and
the JID in the store is that one, so a lookup by the other spelling found
nothing. People also type numbers with a `+`, spaces, dashes and parentheses,
none of which a JID carries.

Pure string work: no database, no bridge call. The callers bind what comes out
of here as SQL parameters.
"""

from __future__ import annotations

import re

# What people put between the digits of a number.
_SEPARATORS = re.compile(r"[\s().-]+")

# 55, a two-digit area code (DDD, never a zero in it), the optional ninth digit
# and the eight-digit subscriber number. Only a subscriber number beginning
# 6-9 has two spellings: those are the mobile ranges that existed before the
# ninth digit, while 2-5 are landlines, which never gained one. Without that
# bound the landline 55 11 3333-4444 and the mobile 55 11 9 3333-4444, two
# different subscribers, would answer for each other.
_BR_MOBILE = re.compile(r"55([1-9]{2})(9?)([6-9][0-9]{7})")


def phone_digits(value: str) -> str | None:
    """The digits of a query that is a phone number and nothing else, or None.

    `+55 (88) 98195-2753` is `5588981952753`. Anything holding a letter, a
    wildcard or an `@` is not a number here, so a name search is left alone.
    """
    compact = _SEPARATORS.sub("", value or "").removeprefix("+")
    return compact if compact.isascii() and compact.isdigit() else None


def br_mobile_alternate(digits: str) -> str | None:
    """The other spelling of a full Brazilian mobile number, or None.

    Only a complete number with the country code has one: twelve digits gain
    the ninth digit, thirteen lose it. A number from any other country, a
    Brazilian landline and a partial number have a single spelling.
    """
    match = _BR_MOBILE.fullmatch(digits)
    if match is None:
        return None
    area, ninth, subscriber = match.groups()
    return f"55{area}{'' if ninth else '9'}{subscriber}"
