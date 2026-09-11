import sys
from pathlib import Path

# 保证 `pytest` 和 `python -m pytest` 都能导入项目根目录的 tool_governance_demo。
ROOT = str(Path(__file__).resolve().parent)
if ROOT not in sys.path:
    sys.path.insert(0, ROOT)
