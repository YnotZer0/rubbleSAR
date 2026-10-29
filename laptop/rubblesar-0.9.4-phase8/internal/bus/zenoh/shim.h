// Thin C shim over zenoh-c 1.x. It hides zenoh-c's owned/loaned/moved
// conventions so the Go side only deals with opaque pointers and int results.
// Only the stable zenoh-c API is used.
#ifndef RUBBLESAR_ZENOH_SHIM_H
#define RUBBLESAR_ZENOH_SHIM_H

#include <stddef.h>
#include <stdint.h>

typedef struct sar_session sar_session;
typedef struct sar_publisher sar_publisher;
typedef struct sar_subscriber sar_subscriber;

// Stage codes returned through sar_open's *stage when it fails.
#define SAR_STAGE_CONFIG 1
#define SAR_STAGE_OPEN 2

const char *sar_zenoh_c_version(void);
void sar_init_logging(const char *fallback_filter);

int sar_open(const char *json5, sar_session **out, int *stage);
int sar_close(sar_session *s);

int sar_declare_publisher(sar_session *s, const char *key, int priority, int block, int express,
                          sar_publisher **out);
int sar_publisher_put(sar_publisher *p, const uint8_t *data, size_t len);
int sar_undeclare_publisher(sar_publisher *p);

int sar_put(sar_session *s, const char *key, const uint8_t *data, size_t len, int priority, int block,
            int express);

int sar_declare_subscriber(sar_session *s, const char *key, uintptr_t ctx, sar_subscriber **out);
int sar_undeclare_subscriber(sar_subscriber *sub);

#endif
