"""
Cross-Instance Isolation Tests

This module verifies data isolation between Feast Feature Store instances and
the Data Registry when they share the same Postgres SQL registry.

Setup:
    - credit-scoring (noAuth, project: credit_scoring_local)
    - driver-ranking  (noAuth, project: driver_ranking)
    - feast-auth      (kubernetes auth, project: feast_auth_project)
    - data-registry   (project: data_registry, protected via feast.dev/protected-project annotation)

All four CRs share one Postgres database as their SQL registry.  The tests
confirm that the protected-project annotation hides the data_registry project
from Feast project listings, and that the Data Registry's SAR-based auth
prevents it from exposing Feast-only projects.
"""

import os

import pytest

FEAST_PROJECTS = {"credit_scoring_local", "driver_ranking", "feast_auth_project"}
SAVED_DATASETS_COUNT = 9
CREDIT_SCORING_SAVED_DATASET_NAMES = {
    "credit_score_training_v1",
    "credit_history_analysis_v1",
    "demographics_profile_v1",
    "comprehensive_credit_dataset_v1",
    "credit_history_service_v1",
    "demographics_service_v1",
    "location_intelligence_service_v1",
    "customer_profile_service_v1",
    "basic_underwriting_service_v1",
}


def _saved_dataset_name(ds):
    """Feast REST serializes SavedDataset as {spec, meta}; name lives in spec."""
    return (ds.get("spec") or {}).get("name") or ds.get("name") or ""


def _saved_dataset_project(ds):
    """Project is top-level on /saved_datasets/all, not on list-by-project proto JSON."""
    return ds.get("project") or (ds.get("spec") or {}).get("project") or ""


@pytest.mark.integration
@pytest.mark.skipif(
    not os.path.exists(os.path.expanduser("~/.kube/config")),
    reason="Kube config not available",
)
class TestCrossInstanceIsolation:
    """Verify data isolation across Feast instances and the Data Registry."""

    # ------------------------------------------------------------------
    # noAuth Feast -> Data Registry isolation
    # ------------------------------------------------------------------

    def test_noauth_feast_cannot_see_dr_project(self, feast_rest_client):
        """The noAuth Feast instance must not expose the protected data_registry project.

        The data_registry project is annotated with feast.dev/protected-project,
        which instructs the Feast operator to hide it from standard project
        listings.  credit_scoring_local must still be visible as a sanity check.
        """
        response = feast_rest_client.get("/projects")
        assert response.status_code == 200

        data = response.json()
        project_names = [p["spec"]["name"] for p in data["projects"]]

        assert "data_registry" not in project_names, (
            "data_registry should be hidden from noAuth Feast project listing"
        )
        assert "credit_scoring_local" in project_names, (
            "credit_scoring_local should be visible on its own Feast instance"
        )

    def test_noauth_feast_cannot_see_dr_saved_datasets(self, feast_rest_client):
        """Saved datasets returned by the noAuth Feast instance must never
        belong to the data_registry project.

        Even though all CRs share the same Postgres registry, the
        protected-project annotation must prevent data_registry artifacts
        from leaking into Feast's /saved_datasets/all response.
        """
        response = feast_rest_client.get("/saved_datasets/all")
        assert response.status_code == 200

        data = response.json()
        saved_datasets = data.get("savedDatasets", [])
        dr_datasets = [
            ds for ds in saved_datasets if _saved_dataset_project(ds) == "data_registry"
        ]

        assert len(dr_datasets) == 0, (
            f"Found {len(dr_datasets)} saved dataset(s) belonging to data_registry "
            f"in noAuth Feast /saved_datasets/all response"
        )

    # ------------------------------------------------------------------
    # Auth-enabled Feast -> Data Registry isolation
    # ------------------------------------------------------------------

    def test_auth_feast_cannot_see_dr_project(self, feast_auth_client):
        """The auth-enabled Feast instance must not expose the protected
        data_registry project, even when queried with an admin token.

        The hiding is driven by the feast.dev/protected-project annotation,
        not by permission checks, so it must hold regardless of the caller's
        authorization level.
        """
        response = feast_auth_client.get("/projects")
        assert response.status_code == 200

        data = response.json()
        project_names = [p["spec"]["name"] for p in data["projects"]]

        assert "data_registry" not in project_names, (
            "data_registry should be hidden from auth Feast project listing"
        )
        assert "feast_auth_project" in project_names, (
            "feast_auth_project should be visible on its own Feast instance"
        )

    def test_auth_feast_cannot_see_dr_saved_datasets(self, feast_auth_client):
        """Saved datasets returned by the auth-enabled Feast instance must only
        belong to feast_auth_project, never to data_registry.

        This confirms that project-scoped queries on the auth instance do not
        leak cross-project data from the shared registry.
        """
        response = feast_auth_client.get(
            "/saved_datasets?project=feast_auth_project&include_relationships=false"
        )
        assert response.status_code == 200

        data = response.json()
        saved_datasets = data.get("savedDatasets", [])

        for ds in saved_datasets:
            name = _saved_dataset_name(ds)
            project = _saved_dataset_project(ds)
            assert name, f"saved dataset missing spec.name: {ds}"
            assert project != "data_registry", (
                f"data_registry saved dataset {name!r} leaked into "
                "auth Feast /saved_datasets"
            )
            if project:
                assert project == "feast_auth_project", (
                    f"Expected project 'feast_auth_project' but got {project!r} "
                    f"for saved dataset {name!r}"
                )

    # ------------------------------------------------------------------
    # Data Registry -> Feast isolation
    # ------------------------------------------------------------------

    def test_dr_cannot_see_feast_projects(self, data_registry_client):
        """The Data Registry must not expose standard Feast projects.

        The Data Registry's /projects endpoint is filtered through Kubernetes
        SubjectAccessReview (SAR).  Since credit_scoring_local, driver_ranking,
        and feast_auth_project are not Kubernetes namespaces that the test
        ServiceAccount has SAR access to in the data-registry sense, they must
        not appear in the response.
        """
        response = data_registry_client.get("/projects")
        assert response.status_code == 200

        data = response.json()
        project_names = [p["spec"]["name"] for p in data.get("projects", [])]

        for feast_project in FEAST_PROJECTS:
            assert feast_project not in project_names, (
                f"Feast project '{feast_project}' should not be visible in "
                f"Data Registry /projects response"
            )

    # ------------------------------------------------------------------
    # Positive / sanity checks
    # ------------------------------------------------------------------

    def test_noauth_feast_projects_shows_expected_only(self, feast_rest_client):
        """The noAuth Feast instance should list exactly the expected projects.

        credit_scoring_local and driver_ranking must be present.
        data_registry must NOT be present (protected-project annotation).
        feast_auth_project may or may not be present -- the noAuth client skips
        permission checks on listing, so a shared-registry project could appear.
        The critical assertion is that data_registry is absent.
        """
        response = feast_rest_client.get("/projects")
        assert response.status_code == 200

        data = response.json()
        project_names = {p["spec"]["name"] for p in data["projects"]}

        assert "credit_scoring_local" in project_names, (
            "credit_scoring_local must be listed by the noAuth Feast instance"
        )
        assert "driver_ranking" in project_names, (
            "driver_ranking must be listed by the noAuth Feast instance"
        )
        assert "data_registry" not in project_names, (
            "data_registry must be hidden from noAuth Feast project listing"
        )

    def test_feast_saved_datasets_only_own_project(self, feast_rest_client):
        """Project-scoped saved-dataset queries must return only datasets
        belonging to the requested project.

        Querying credit_scoring_local should return exactly 9 datasets, all
        named as credit-scoring training sets. List-by-project proto JSON
        has no top-level project field; isolation is the name set plus any
        project the API does send. No data_registry leakage is allowed.
        """
        response = feast_rest_client.get(
            "/saved_datasets?project=credit_scoring_local&include_relationships=false"
        )
        assert response.status_code == 200

        data = response.json()
        saved_datasets = data.get("savedDatasets", [])

        assert len(saved_datasets) == SAVED_DATASETS_COUNT, (
            f"Expected {SAVED_DATASETS_COUNT} saved datasets for "
            f"credit_scoring_local but got {len(saved_datasets)}"
        )

        actual_names = {_saved_dataset_name(ds) for ds in saved_datasets}
        assert actual_names == CREDIT_SCORING_SAVED_DATASET_NAMES, (
            f"credit_scoring_local saved datasets mismatch: {actual_names}"
        )

        for ds in saved_datasets:
            name = _saved_dataset_name(ds)
            project = _saved_dataset_project(ds)
            assert project != "data_registry", (
                f"data_registry saved dataset {name!r} leaked into "
                "credit_scoring_local /saved_datasets"
            )
            if project:
                assert project == "credit_scoring_local", (
                    f"Expected project 'credit_scoring_local' but got "
                    f"{project!r} for saved dataset {name!r}"
                )
