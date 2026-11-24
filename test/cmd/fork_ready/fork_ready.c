// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <unistd.h>

static const char ready_info[] = "ready\n";

static void die(const char* msg) {
  fprintf(stderr, "%s: %s\n", msg, strerror(errno));
  _exit(1);
}

static void die_msg(const char* msg) {
  fprintf(stderr, "%s\n", msg);
  _exit(1);
}

static void write_all(int fd, const char* buf, size_t len) {
  size_t done = 0;
  while (done < len) {
    ssize_t n = write(fd, buf + done, len - done);
    if (n < 0) {
      if (errno == EINTR) {
        continue;
      }
      die("write");
    }
    if (n == 0) {
      die_msg("write returned 0");
    }
    done += (size_t)n;
  }
}

int main(void) {
  const char* wait_checkpoint = getenv("WAIT_CHECKPOINT");
  if (wait_checkpoint != NULL && strcmp(wait_checkpoint, "1") == 0) {
    int fd = open("/proc/gvisor/checkpoint", O_RDWR);
    if (fd < 0) {
      die("open checkpoint");
    }
    write_all(fd, "1", 1);
    char result[8];
    size_t done = 0;
    while (done < sizeof(result)) {
      ssize_t n = read(fd, result + done, sizeof(result) - done);
      if (n < 0 && errno == EINTR) {
        continue;
      }
      if (n <= 0) {
        die_msg("incomplete checkpoint result");
      }
      done += (size_t)n;
    }
    if (memcmp(result, "restore\n", sizeof(result)) != 0) {
      die_msg("unexpected checkpoint result");
    }
    if (close(fd) < 0) {
      die("close checkpoint");
    }
  }

  const char* host = getenv("READY_HOST");
  const char* port_str = getenv("READY_PORT");
  if (host == NULL || port_str == NULL) {
    die_msg("READY_HOST and READY_PORT must be set");
  }

  char* end = NULL;
  long port = strtol(port_str, &end, 10);
  if (end == port_str || *end != '\0' || port <= 0 || port > 65535) {
    die_msg("invalid READY_PORT");
  }

  struct sockaddr_in addr;
  memset(&addr, 0, sizeof(addr));
  addr.sin_family = AF_INET;
  addr.sin_port = htons((uint16_t)port);
  if (inet_pton(AF_INET, host, &addr.sin_addr) != 1) {
    die_msg("invalid READY_HOST");
  }

  int fd = socket(AF_INET, SOCK_STREAM, 0);
  if (fd < 0) {
    die("socket");
  }
  if (connect(fd, (struct sockaddr*)&addr, sizeof(addr)) < 0) {
    die("connect");
  }
  write_all(fd, ready_info, sizeof(ready_info) - 1);
  char ack;
  ssize_t n;
  do {
    n = read(fd, &ack, sizeof(ack));
  } while (n < 0 && errno == EINTR);
  if (n < 0) {
    die("read ready acknowledgment");
  }
  if (n != 1 || ack != '\n') {
    die_msg("invalid ready acknowledgment");
  }
  if (close(fd) < 0) {
    die("close socket");
  }
  return 0;
}
