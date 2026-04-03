from __future__ import annotations

import json
import threading
from dataclasses import dataclass
from pathlib import Path

from app.config import settings

MODELS_FILE = "models.json"


@dataclass
class ModelAlias:
    alias: str
    target: str


class ModelManager:
    def __init__(self) -> None:
        self._aliases: list[ModelAlias] = []
        self._lock = threading.Lock()

    def load(self, path: str | Path | None = None) -> None:
        path = Path(path or MODELS_FILE)
        if not path.exists():
            with open(path, "w") as f:
                json.dump([], f)
            return
        with open(path) as f:
            data = json.load(f)
        with self._lock:
            self._aliases = [
                ModelAlias(alias=item["alias"], target=item["target"])
                for item in data
                if isinstance(item, dict) and "alias" in item and "target" in item
            ]

    def _save(self) -> None:
        path = Path(MODELS_FILE)
        data = [{"alias": a.alias, "target": a.target} for a in self._aliases]
        with open(path, "w") as f:
            json.dump(data, f, indent=2, ensure_ascii=False)
            f.write("\n")

    def resolve(self, model: str) -> tuple[str, str | None]:
        """Returns (target_model, alias_used_or_None)."""
        with self._lock:
            for a in self._aliases:
                if a.alias == model:
                    return a.target, a.alias
        return model, None

    def reverse(self, target: str) -> str | None:
        """Find alias for a target model (for response rewriting)."""
        with self._lock:
            for a in self._aliases:
                if a.target == target:
                    return a.alias
        return None

    def get_all(self) -> list[dict]:
        with self._lock:
            return [{"alias": a.alias, "target": a.target} for a in self._aliases]

    def add(self, alias: str, target: str) -> dict:
        with self._lock:
            for a in self._aliases:
                if a.alias == alias:
                    raise ValueError(f"Alias '{alias}' already exists")
            entry = ModelAlias(alias=alias, target=target)
            self._aliases.append(entry)
            self._save()
            return {"alias": entry.alias, "target": entry.target}

    def update(self, alias: str, *, new_alias: str | None = None, new_target: str | None = None) -> dict:
        with self._lock:
            target_entry = None
            for a in self._aliases:
                if a.alias == alias:
                    target_entry = a
                    break
            if target_entry is None:
                raise KeyError(f"Alias '{alias}' not found")
            if new_alias is not None and new_alias != alias:
                for a in self._aliases:
                    if a.alias == new_alias:
                        raise ValueError(f"Alias '{new_alias}' already exists")
                target_entry.alias = new_alias
            if new_target is not None:
                target_entry.target = new_target
            self._save()
            return {"alias": target_entry.alias, "target": target_entry.target}

    def remove(self, alias: str) -> None:
        with self._lock:
            before = len(self._aliases)
            self._aliases = [a for a in self._aliases if a.alias != alias]
            if len(self._aliases) == before:
                raise KeyError(f"Alias '{alias}' not found")
            self._save()


model_manager = ModelManager()
