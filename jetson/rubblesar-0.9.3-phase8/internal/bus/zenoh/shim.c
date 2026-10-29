//go:build !nozenoh

#include "shim.h"

#include <stdlib.h>
#include <string.h>
#include <zenoh.h>

#include "_cgo_export.h"

struct sar_session {
  z_owned_session_t s;
};
struct sar_publisher {
  z_owned_publisher_t p;
};
struct sar_subscriber {
  z_owned_subscriber_t s;
};

const char *sar_zenoh_c_version(void) { return ZENOH_C; }

void sar_init_logging(const char *fallback_filter) { zc_init_log_from_env_or(fallback_filter); }

int sar_open(const char *json5, sar_session **out, int *stage) {
  z_owned_config_t cfg;
  int rc = zc_config_from_str(&cfg, json5);
  if (rc != 0) {
    *stage = SAR_STAGE_CONFIG;
    return rc;
  }
  sar_session *s = (sar_session *)calloc(1, sizeof(sar_session));
  if (!s) {
    z_config_drop(z_config_move(&cfg));
    *stage = SAR_STAGE_OPEN;
    return -128;
  }
  rc = z_open(&s->s, z_config_move(&cfg), NULL);
  if (rc != 0) {
    free(s);
    *stage = SAR_STAGE_OPEN;
    return rc;
  }
  *out = s;
  return 0;
}

int sar_close(sar_session *s) {
  if (!s) return 0;
  int rc = z_close(z_session_loan_mut(&s->s), NULL);
  z_session_drop(z_session_move(&s->s));
  free(s);
  return rc;
}

static void pub_opts(z_publisher_options_t *o, int priority, int block, int express) {
  z_publisher_options_default(o);
  o->priority = (enum z_priority_t)priority;
  o->congestion_control = block ? Z_CONGESTION_CONTROL_BLOCK : Z_CONGESTION_CONTROL_DROP;
  o->is_express = express != 0;
}

int sar_declare_publisher(sar_session *s, const char *key, int priority, int block, int express,
                          sar_publisher **out) {
  z_view_keyexpr_t ke;
  int rc = z_view_keyexpr_from_str(&ke, key);
  if (rc != 0) return rc;
  sar_publisher *p = (sar_publisher *)calloc(1, sizeof(sar_publisher));
  if (!p) return -128;
  z_publisher_options_t o;
  pub_opts(&o, priority, block, express);
  rc = z_declare_publisher(z_session_loan(&s->s), &p->p, z_view_keyexpr_loan(&ke), &o);
  if (rc != 0) {
    free(p);
    return rc;
  }
  *out = p;
  return 0;
}

int sar_publisher_put(sar_publisher *p, const uint8_t *data, size_t len) {
  z_owned_bytes_t payload;
  int rc = z_bytes_copy_from_buf(&payload, data, len);
  if (rc != 0) return rc;
  return z_publisher_put(z_publisher_loan(&p->p), z_bytes_move(&payload), NULL);
}

int sar_undeclare_publisher(sar_publisher *p) {
  if (!p) return 0;
  int rc = z_undeclare_publisher(z_publisher_move(&p->p));
  free(p);
  return rc;
}

int sar_put(sar_session *s, const char *key, const uint8_t *data, size_t len, int priority, int block,
            int express) {
  z_view_keyexpr_t ke;
  int rc = z_view_keyexpr_from_str(&ke, key);
  if (rc != 0) return rc;
  z_owned_bytes_t payload;
  rc = z_bytes_copy_from_buf(&payload, data, len);
  if (rc != 0) return rc;
  z_put_options_t o;
  z_put_options_default(&o);
  o.priority = (enum z_priority_t)priority;
  o.congestion_control = block ? Z_CONGESTION_CONTROL_BLOCK : Z_CONGESTION_CONTROL_DROP;
  o.is_express = express != 0;
  return z_put(z_session_loan(&s->s), z_view_keyexpr_loan(&ke), z_bytes_move(&payload), &o);
}

static void on_sample(z_loaned_sample_t *sample, void *ctx) {
  if (z_sample_kind(sample) != Z_SAMPLE_KIND_PUT) return;
  z_view_string_t ks;
  z_keyexpr_as_view_string(z_sample_keyexpr(sample), &ks);
  const z_loaned_string_t *kl = z_view_string_loan(&ks);
  z_owned_slice_t sl;
  if (z_bytes_to_slice(z_sample_payload(sample), &sl) != 0) return;
  const z_loaned_slice_t *l = z_slice_loan(&sl);
  sarGoOnSample((uintptr_t)ctx, (char *)z_string_data(kl), z_string_len(kl), (uint8_t *)z_slice_data(l),
                z_slice_len(l));
  z_slice_drop(z_slice_move(&sl));
}

static void on_drop(void *ctx) { sarGoOnDrop((uintptr_t)ctx); }

int sar_declare_subscriber(sar_session *s, const char *key, uintptr_t ctx, sar_subscriber **out) {
  z_view_keyexpr_t ke;
  int rc = z_view_keyexpr_from_str(&ke, key);
  if (rc != 0) return rc;
  sar_subscriber *sub = (sar_subscriber *)calloc(1, sizeof(sar_subscriber));
  if (!sub) return -128;
  z_owned_closure_sample_t cb;
  z_closure_sample(&cb, on_sample, on_drop, (void *)ctx);
  rc = z_declare_subscriber(z_session_loan(&s->s), &sub->s, z_view_keyexpr_loan(&ke),
                            z_closure_sample_move(&cb), NULL);
  if (rc != 0) {
    free(sub);
    return rc;
  }
  *out = sub;
  return 0;
}

int sar_undeclare_subscriber(sar_subscriber *sub) {
  if (!sub) return 0;
  int rc = z_undeclare_subscriber(z_subscriber_move(&sub->s));
  free(sub);
  return rc;
}
