"""The spellings of one phone number, for the contact lookups (issue #444).

A Brazilian mobile is written two ways: with the ninth digit that was put in
front of every mobile subscriber number (55 88 9 7777-6666) and without it
(55 88 7777-6666). WhatsApp registers the account under one of the two, and
the JID in the store is that one, so a lookup by the other spelling found
nothing. People also type numbers with a `+`, spaces, dashes and parentheses,
none of which a JID carries, and a number copied out of WhatsApp comes with a
non-breaking hyphen and directional marks around it.

Pure string work: no database, no bridge call. The callers bind what comes out
of here as SQL parameters.
"""

from __future__ import annotations

import re
import unicodedata

# Below a local number, digits and punctuation are not a phone number: "1.5"
# and "(11)" stay the name searches they were.
MIN_PHONE_DIGITS = 7

# 55, a two-digit area code (DDD, never a zero in it), the optional ninth digit
# and the eight-digit subscriber number. Only a subscriber number beginning
# 6-9 has two spellings: those are the mobile ranges that existed before the
# ninth digit, while 2-5 are landlines, which never gained one. Without that
# bound the landline 55 11 3333-4444 and the mobile 55 11 9 3333-4444, two
# different subscribers, would answer for each other.
_BR_MOBILE = re.compile(r"55([1-9]{2})(9?)([6-9][0-9]{7})")


def _is_separator(char: str) -> bool:
    """What sits between the digits of a number: spaces, any dash, dots, parentheses.

    Format characters count too (category Cf: the directional marks WhatsApp
    wraps a displayed number in, zero-width spaces).
    """
    return char in "()." or char.isspace() or unicodedata.category(char) in ("Pd", "Cf")


def phone_digits(value: str) -> str | None:
    """The digits of a query that is a phone number and nothing else, or None.

    `+55 (88) 97777-6666` is `5588977776666`. Anything holding a letter, a
    wildcard or an `@` is not a number here, and neither is anything shorter
    than seven digits, so a name search is left alone.
    """
    compact = "".join(char for char in value or "" if not _is_separator(char)).removeprefix("+")
    if len(compact) < MIN_PHONE_DIGITS or not (compact.isascii() and compact.isdigit()):
        return None
    return compact


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
