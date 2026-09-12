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

// TestTemplateForkState runs this workload in the seed and its forked children.
#define _DEFAULT_SOURCE

#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/random.h>
#include <time.h>
#include <unistd.h>

static void fail(const char* what) {
  perror(what);
  exit(EXIT_FAILURE);
}

static void wait_for_fork(void) {
  int fd = open("/proc/gvisor/checkpoint", O_RDWR);
  if (fd < 0 || write(fd, "1", 1) != 1) {
    fail("request checkpoint");
  }

  const char expected[] = "restore\n";
  char result[sizeof(expected) - 1];
  ssize_t n = read(fd, result, sizeof(result));
  if (n < 0) {
    fail("read checkpoint");
  }
  if ((size_t)n != sizeof(result) ||
      memcmp(result, expected, sizeof(result)) != 0) {
    fprintf(stderr, "unexpected checkpoint result\n");
    exit(EXIT_FAILURE);
  }
  close(fd);
}

static void check_mappings(const unsigned char* memory, unsigned char anon_byte,
                           const unsigned char* mapped, unsigned char file_byte,
                           size_t size) {
  for (size_t i = 0; i < size; ++i) {
    if (memory[i] != anon_byte || mapped[i] != file_byte) {
      fprintf(stderr, "unexpected mapping contents at offset %zu\n", i);
      exit(EXIT_FAILURE);
    }
  }
}

int main(void) {
  // Populate both mappings before freezing so every child inherits these bytes.
  const size_t size = 16 * 1024 * 1024;
  unsigned char* memory = mmap(NULL, size, PROT_READ | PROT_WRITE,
                               MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (memory == MAP_FAILED) {
    fail("anonymous mmap");
  }
  memset(memory, 0xa5, size);

  int data = open("/state-data", O_CREAT | O_RDWR, 0600);
  if (data < 0 || ftruncate(data, size) != 0) {
    fail("prepare state file");
  }
  unsigned char* mapped =
      mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, data, 0);
  if (mapped == MAP_FAILED) {
    fail("file mmap");
  }
  memset(mapped, 0x5a, size);

  const clockid_t clocks[] = {CLOCK_REALTIME, CLOCK_MONOTONIC,
                              CLOCK_MONOTONIC_RAW};
  enum { clock_count = sizeof(clocks) / sizeof(clocks[0]) };
  struct timespec before[clock_count], after[clock_count];
  for (size_t i = 0; i < clock_count; ++i) {
    if (clock_gettime(clocks[i], &before[i]) != 0) {
      fail("initial clock");
    }
  }

  wait_for_fork();

  for (size_t i = 0; i < clock_count; ++i) {
    if (clock_gettime(clocks[i], &after[i]) != 0) {
      fail("resumed clock");
    }
    if (after[i].tv_sec < before[i].tv_sec ||
        (after[i].tv_sec == before[i].tv_sec &&
         after[i].tv_nsec < before[i].tv_nsec)) {
      fprintf(stderr, "clock %zu went backwards\n", i);
      return EXIT_FAILURE;
    }
  }
  check_mappings(memory, 0xa5, mapped, 0x5a, size);

  // Give siblings time to write their own patterns before checking ours.
  uint64_t token[2];
  if (getrandom(token, sizeof(token), 0) != sizeof(token)) {
    fail("getrandom");
  }
  memset(memory, (unsigned char)token[0], size);
  memset(mapped, (unsigned char)token[1], size);
  struct timespec delay = {.tv_nsec = 20000000};
  while (nanosleep(&delay, &delay) != 0) {
    if (errno != EINTR) {
      fail("nanosleep");
    }
  }
  check_mappings(memory, (unsigned char)token[0], mapped,
                 (unsigned char)token[1], size);

  // The Go test compares tokens across children and realtime with the host.
  int64_t realtime_ns =
      (int64_t)after[0].tv_sec * INT64_C(1000000000) + after[0].tv_nsec;
  printf("%016" PRIx64 "%016" PRIx64 " %" PRId64 "\n", token[0], token[1],
         realtime_ns);
  munmap(mapped, size);
  munmap(memory, size);
  close(data);
  return EXIT_SUCCESS;
}
