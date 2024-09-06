# WOT Relay

WOT Relay is a Nostr relay specialized for managing a web of trust based on trusted public keys. This relay is designed to run on localhost, with events filtered by a defined trust network.

## Prerequisites

- **Go**: Ensure you have Go installed on your system. You can download it from [here](https://golang.org/dl/).

## Setup Instructions

Follow these steps to get the WOT Relay running on your local machine:

### 1. Clone the repository

```bash
git clone https://github.com/bitvora/wot-relay.git
cd wot-relay
```

### 2. Copy `.env.example` to `.env`

You'll need to create an `.env` file based on the example provided in the repository.

```bash
cp .env.example .env
```

### 3. Set your environment variables

Open the `.env` file and set the necessary environment variables. Example variables include:

```bash
RELAY_NAME="YourRelayName"
RELAY_PUBKEY="YourPublicKey"
RELAY_DESCRIPTION="Your relay description"
DB_PATH="/path/to/your/database"
```

### 4. Build the project

Run the following command to build the relay:

```bash
go build
```

### 5. Create a Systemd Service (optional)

To have the relay run as a service, create a systemd unit file. Here's an example:

1. Create the file:

```bash
sudo nano /etc/systemd/system/wot-relay.service
```

2. Add the following contents:

```ini
[Unit]
Description=WOT Relay Service
After=network.target

[Service]
ExecStart=/path/to/wot-relay
WorkingDirectory=/path/to/wot-relay
Restart=always
EnvironmentFile=/path/to/.env

[Install]
WantedBy=multi-user.target
```

Replace `/path/to/` with the actual paths where you cloned the repository and stored the `.env` file.

3. Reload systemd to recognize the new service:

```bash
sudo systemctl daemon-reload
```

4. Start the service:

```bash
sudo systemctl start wot-relay
```

5. (Optional) Enable the service to start on boot:

```bash
sudo systemctl enable wot-relay
```

### 6. Access the relay

Once everything is set up, the relay will be running on `localhost:3334`.

```bash
http://localhost:3334
```

## License

This project is licensed under the MIT License.
