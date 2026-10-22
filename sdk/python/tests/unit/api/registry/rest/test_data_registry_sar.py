# Copyright 2026 The Feast Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import sys
from types import SimpleNamespace
from unittest.mock import Mock

import pytest

from feast.api.registry.rest import data_registry_sar


def test_sar_check_uses_subject_access_review(monkeypatch):
    captured = {}

    class FakeTokenReviewSpec:
        def __init__(self, *, token):
            self.token = token

    class FakeTokenReview:
        def __init__(self, *, spec):
            self.spec = spec

    class FakeResourceAttributes:
        def __init__(self, **kwargs):
            self.__dict__.update(kwargs)

    class FakeSubjectAccessReviewSpec:
        def __init__(self, **kwargs):
            self.__dict__.update(kwargs)

    class FakeSubjectAccessReview:
        def __init__(self, *, spec):
            self.spec = spec
            captured["sar"] = self

    class FakeAuthenticationV1Api:
        def create_token_review(self, token_review):
            captured["token_review"] = token_review
            return SimpleNamespace(
                status=SimpleNamespace(
                    authenticated=True,
                    user=SimpleNamespace(username="alice", groups=["team-a"]),
                )
            )

    fake_client = SimpleNamespace(
        V1TokenReviewSpec=FakeTokenReviewSpec,
        V1TokenReview=FakeTokenReview,
        V1ResourceAttributes=FakeResourceAttributes,
        V1SubjectAccessReviewSpec=FakeSubjectAccessReviewSpec,
        V1SubjectAccessReview=FakeSubjectAccessReview,
        AuthenticationV1Api=FakeAuthenticationV1Api,
    )
    monkeypatch.setitem(sys.modules, "kubernetes", SimpleNamespace(client=fake_client))
    monkeypatch.setattr(
        data_registry_sar, "_SAR_API_GROUP", "dataregistry.opendatahub.io"
    )

    authz_api = Mock()
    authz_api.create_subject_access_review.return_value = SimpleNamespace(
        status=SimpleNamespace(allowed=True)
    )

    assert data_registry_sar._do_sar_check(
        authz_api, "bearer-token", "demo-user-1", "namespaces", "list"
    )

    assert captured["token_review"].spec.token == "bearer-token"
    sar_spec = captured["sar"].spec
    assert sar_spec.user == "alice"
    assert sar_spec.groups == ["team-a"]
    assert sar_spec.resource_attributes.namespace == "demo-user-1"
    assert sar_spec.resource_attributes.group == "dataregistry.opendatahub.io"
    assert sar_spec.resource_attributes.resource == "namespaces"
    assert sar_spec.resource_attributes.verb == "list"
    authz_api.create_subject_access_review.assert_called_once_with(captured["sar"])


@pytest.mark.parametrize(
    "token_status",
    [
        None,
        SimpleNamespace(authenticated=False, user=None),
        SimpleNamespace(authenticated=True, user=None),
        SimpleNamespace(
            authenticated=True,
            user=SimpleNamespace(username=None, groups=[]),
        ),
    ],
)
def test_sar_check_rejects_incomplete_token_review(monkeypatch, token_status):
    class FakeTokenReviewSpec:
        def __init__(self, *, token):
            self.token = token

    class FakeTokenReview:
        def __init__(self, *, spec):
            self.spec = spec

    class FakeAuthenticationV1Api:
        def create_token_review(self, token_review):
            return SimpleNamespace(status=token_status)

    fake_client = SimpleNamespace(
        V1TokenReviewSpec=FakeTokenReviewSpec,
        V1TokenReview=FakeTokenReview,
        AuthenticationV1Api=FakeAuthenticationV1Api,
    )
    monkeypatch.setitem(sys.modules, "kubernetes", SimpleNamespace(client=fake_client))

    authz_api = Mock()

    assert not data_registry_sar._do_sar_check(
        authz_api, "bearer-token", "demo-user-1", "namespaces", "list"
    )
    authz_api.create_subject_access_review.assert_not_called()
