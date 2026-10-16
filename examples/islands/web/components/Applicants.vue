<script setup lang="ts">
import { useLive } from 'vuka:live'
import type { Applicants } from 'vuka:live'

const live = useLive<Applicants>()
</script>

<template>
  <input v-model="live.q.value" placeholder="Search applicants" />
  <small v-if="live.error">{{ live.error.fields?.q ?? live.error.message }}</small>
  <ul>
    <li v-for="a in live.rows.value ?? []" :key="a.name">{{ a.name }} — {{ a.role }}</li>
  </ul>
</template>
