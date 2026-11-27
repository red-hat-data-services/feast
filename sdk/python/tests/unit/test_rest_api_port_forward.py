from contextlib import nullcontext
from unittest.mock import Mock, mock_open, patch

import pytest

from tests.integration.rest_api import support


def test_port_forward_service_uses_https_and_stops_on_teardown(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    process = Mock()
    process.poll.return_value = None
    commands = []
    stderr_file = Mock()
    stderr_file.name = "/tmp/kubectl-port-forward-test.log"

    def start(command: list[str], **kwargs: object) -> Mock:
        commands.append(command)
        return process

    monkeypatch.setattr(support.subprocess, "Popen", start)
    monkeypatch.setattr(
        support.socket, "create_connection", lambda *args, **kwargs: nullcontext()
    )
    monkeypatch.setattr(
        support.tempfile,
        "NamedTemporaryFile",
        lambda **kwargs: stderr_file,
    )
    monkeypatch.setattr(support.os, "unlink", lambda path: None)

    with support.port_forward_service("test-namespace", "registry-rest") as url:
        assert url.startswith("https://127.0.0.1:")
        assert commands == [
            [
                "kubectl",
                "port-forward",
                "--address=127.0.0.1",
                "service/registry-rest",
                f"{url.rsplit(':', 1)[1]}:https",
                "-n",
                "test-namespace",
            ]
        ]

    process.terminate.assert_called_once_with()
    process.wait.assert_called_once_with(timeout=5)
    stderr_file.close.assert_called()


def test_port_forward_service_reports_startup_failure(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    process = Mock()
    process.poll.return_value = 1
    stderr_file = Mock()
    stderr_file.name = "/tmp/kubectl-port-forward-fail.log"

    monkeypatch.setattr(support.subprocess, "Popen", lambda *args, **kwargs: process)
    monkeypatch.setattr(
        support.tempfile,
        "NamedTemporaryFile",
        lambda **kwargs: stderr_file,
    )
    monkeypatch.setattr(support.os, "unlink", lambda path: None)

    with (
        patch("builtins.open", mock_open(read_data="service unavailable")),
        pytest.raises(RuntimeError, match="service unavailable"),
    ):
        with support.port_forward_service("test-namespace", "registry-rest"):
            pass

    process.terminate.assert_not_called()
    stderr_file.close.assert_called()
