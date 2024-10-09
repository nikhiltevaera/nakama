# Stage 1: Build the plugin
FROM heroiclabs/nakama-pluginbuilder:3.23.0 AS builder
ENV GO111MODULE on 
ENV CGO_ENABLED 1

WORKDIR /backend
COPY . .

# Build the plugin
RUN go build --trimpath --mod=vendor --buildmode=plugin -o ./backend.so

# Stage 2: Create the final image
FROM heroiclabs/nakama:3.23.0

COPY --from=builder /backend/backend.so /nakama/data/modules/
COPY --from=builder /backend/config.yml /nakama/data/

