# Use Golang image based on Debian Bookworm
FROM golang:bookworm

# Set the working directory within the container
WORKDIR /app

# Clone the repository
RUN git clone https://github.com/bitvora/wot-relay . 

# Download Go module dependencies
RUN go mod download

# Write the .env file
RUN touch .env && \
    echo "RELAY_NAME=${RELAY_NAME}" >> .env && \
    echo "RELAY_PUBKEY=${RELAY_PUBKEY}" >> .env && \
    echo "RELAY_DESCRIPTION=${RELAY_DESCRIPTION}" >> .env && \
    echo "DB_PATH=${DB_PATH}" >> .env

# Build the Go application
RUN go build -o main .

# Expose the port that the application will run on
EXPOSE 3334

# Set the command to run the executable
CMD ["./main"]
