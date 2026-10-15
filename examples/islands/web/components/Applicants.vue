<script setup lang="ts">
import { ref } from 'vue'
import { useLive } from 'vuka:live'
import type { Applicants } from 'vuka:live'

const props = defineProps<Applicants>()
const live = useLive<Applicants>()
const q = ref(props.q)
</script>

<template>
  <form @submit.prevent="live.search(q)">
    <input v-model="q" placeholder="Search applicants" />
    <button :disabled="live.pending">Search</button>
    <small v-if="live.error">{{ live.error.fields?.q ?? live.error.message }}</small>
  </form>
  <ul>
    <li v-for="a in rows" :key="a.name">{{ a.name }} — {{ a.role }}</li>
  </ul>
</template>
