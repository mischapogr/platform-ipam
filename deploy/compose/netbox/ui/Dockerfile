FROM docker.io/golang:1.26.8-bookworm AS platform
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /platform-ipam ./cmd/platform-ipam

FROM docker.io/netboxcommunity/netbox:v4.7.1-5.1.1@sha256:59e3e5954d0243d8648f2fa7ba82cf7c7770aaeef392026cca8b21cd67411e74
COPY --from=platform /platform-ipam /usr/local/bin/platform-ipam
COPY scripts/aws/address-plan.py /opt/platform-ipam/address-plan.py
COPY scripts/aws/pilot-evidence.py /opt/platform-ipam/pilot-evidence.py
COPY deploy/compose/netbox/ui/platform_ipam_workspace /opt/netbox/netbox/platform_ipam_workspace
COPY examples/migration-workspace/HUNDRED_ACCOUNT_DEMO/pilot-overview.json /opt/netbox/netbox/platform_ipam_workspace/sample_100_pilot.json
COPY examples/migration-workspace/HUNDRED_ACCOUNT_DEMO /opt/netbox/netbox/platform_ipam_workspace/fixtures/100
COPY examples/migration-workspace/FIVE_HUNDRED_ACCOUNT_DEMO /opt/netbox/netbox/platform_ipam_workspace/fixtures/500
