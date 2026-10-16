from typing import Literal, Optional

from pydantic import StrictBool, StrictStr

from feast.infra.feature_servers.base_config import BaseFeatureServerConfig


class LocalFeatureServerConfig(BaseFeatureServerConfig):
    # Feature server type selector.
    type: Literal["local"] = "local"

    # The endpoint definition for transformation_service
    transformation_service_endpoint: str = "localhost:6569"

    # Whether to dial transformation_service over TLS
    transformation_service_tls: StrictBool = False

    # PEM CA bundle trusted in addition to the system roots, when TLS is enabled
    transformation_service_cert: Optional[StrictStr] = None
