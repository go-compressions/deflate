/* ldcheck: decode raw-DEFLATE files with libdeflate, for the differential test
   in ../../../libdeflate_test.go (build tag "libdeflate").

   usage: ldcheck <cap-bytes> <file>...
   For each file prints "<rc> <nbytes> <fnv1a64-hex>" on one line, where rc is
   0 ok, 1 bad data, 3 output past the cap (libdeflate's own result codes). */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include "libdeflate.h"

static unsigned char *slurp(const char *p, size_t *n) {
	FILE *f = fopen(p, "rb");
	if (!f) { perror(p); exit(2); }
	fseek(f, 0, SEEK_END); *n = (size_t)ftell(f); fseek(f, 0, SEEK_SET);
	unsigned char *b = malloc(*n ? *n : 1);
	if (*n && fread(b, 1, *n, f) != *n) exit(2);
	fclose(f);
	return b;
}

int main(int argc, char **argv) {
	if (argc < 3) return 2;
	size_t cap = (size_t)strtoull(argv[1], NULL, 10);
	unsigned char *out = malloc(cap);
	struct libdeflate_decompressor *d = libdeflate_alloc_decompressor();
	for (int i = 2; i < argc; i++) {
		size_t zn, got = 0;
		unsigned char *z = slurp(argv[i], &zn);
		int rc = (int)libdeflate_deflate_decompress(d, z, zn, out, cap, &got);
		uint64_t h = 0xcbf29ce484222325ULL;
		if (rc == 0) {
			for (size_t k = 0; k < got; k++) { h ^= out[k]; h *= 0x100000001b3ULL; }
		} else {
			got = 0;
		}
		printf("%d %zu %016llx\n", rc, got, (unsigned long long)h);
		free(z);
	}
	return 0;
}
