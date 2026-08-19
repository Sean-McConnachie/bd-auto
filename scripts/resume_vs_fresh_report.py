#!/usr/bin/env python3
"""The resume-versus-fresh arms of scripts/ab_report.py.

Kept as its own entry point because the measurement it renders is cited by name
in plans/headless.md, in the README and in the max_rounds comment in
internal/config: a reader following any of those to re-run it should find the
command they were given, not a note about where it moved to.
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from ab_report import main  # noqa: E402

if __name__ == "__main__":
    sys.exit(main([
        sys.argv[1] if len(sys.argv) > 1 else "reports",
        "--arms", "fresh,resume",
        "--labels", "max_rounds 1, retry 3|max_rounds 4, retry 1",
        "--title", "Resume versus fresh: measured",
    ]))
